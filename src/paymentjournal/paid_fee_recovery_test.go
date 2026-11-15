package paymentjournal

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"
)

// Exercise real process death around each new durable transition. An exit
// after rename observes the filesystem's current bytes, not power-loss safety.
func TestPaidFeeProcessExitBoundaries(t *testing.T) {
	for _, transition := range []string{"record", "resolve"} {
		for _, phase := range []string{"before-rename", "after-rename", "after-return"} {
			t.Run(transition+"/"+phase, func(t *testing.T) {
				s, dir := openTest(t)
				budget := Budget{LimitMSat: 2200}
				a, err := s.Prepare(intent("settled"), budget)
				if err != nil {
					t.Fatal(err)
				}
				b, err := s.Prepare(intent("prepared"), budget)
				if err != nil {
					t.Fatal(err)
				}
				a, err = s.SubmitOutgoing(a.ID, a.Version, budget)
				if err != nil {
					t.Fatal(err)
				}
				if transition == "resolve" {
					if _, err = s.RecordOutgoingPaid(a.ID, a.Version); err != nil {
						t.Fatal(err)
					}
				}
				if err = s.Close(); err != nil {
					t.Fatal(err)
				}

				cmd := exec.Command(os.Args[0], "-test.run=^TestPaidFeeProcessExitHelper$")
				cmd.Env = append(os.Environ(), "TG_PAID_FEE_TEST_DIR="+dir,
					"TG_PAID_FEE_TEST_TRANSITION="+transition, "TG_PAID_FEE_TEST_PHASE="+phase)
				wantCode := map[string]int{"before-rename": 37, "after-rename": 38, "after-return": 39}[phase]
				if code := helperExitCode(t, cmd.Run()); code != wantCode {
					t.Fatalf("helper exit = %d, want %d", code, wantCode)
				}
				again, err := Open(dir)
				if err != nil {
					t.Fatal("reopen after process death:", err)
				}
				defer again.Close()
				got, err := again.Get(a.ID)
				if err != nil {
					t.Fatal(err)
				}
				want := PaidFeePending
				if transition == "record" && phase == "before-rename" {
					want = Unknown
				} else if transition == "resolve" && phase != "before-rename" {
					want = Paid
				}
				wantFee := uint64(0)
				if want == Paid {
					wantFee = 102
				}
				if got.State != want || got.ActualFeeMSat != wantFee || got.PaymentHash != a.PaymentHash || got.WalletID != a.WalletID {
					t.Fatalf("recovered %+v, want %s fee=%d with immutable payment identity", got, want, wantFee)
				}
				if _, err = again.SubmitOutgoing(got.ID, got.Version, budget); err == nil {
					t.Fatal("recovered payment could be resubmitted")
				}
				if got.State == PaidFeePending {
					future := Budget{LimitMSat: 1 << 20, Since: time.Now().Add(time.Hour)}
					if _, err = again.Prepare(intent("new"), future); !errors.Is(err, ErrUnresolvedFee) {
						t.Fatalf("recovered marker allowed new admission: %v", err)
					}
					if _, err = again.SubmitOutgoing(b.ID, b.Version, future); !errors.Is(err, ErrUnresolvedFee) {
						t.Fatalf("recovered marker allowed prepared submission: %v", err)
					}
				}
				if got.State == Paid {
					one := intent("one-more")
					one.AmountMSat, one.FeeReserveMSat = 1, 0
					if _, err = again.Prepare(one, Budget{LimitMSat: 2202}); err == nil {
						t.Fatal("recovered full expense manufactured headroom")
					}
				}
			})
		}
	}
}

func TestPaidFeeProcessExitHelper(t *testing.T) {
	dir := os.Getenv("TG_PAID_FEE_TEST_DIR")
	if dir == "" {
		return
	}
	s, err := Open(dir)
	if err != nil {
		os.Exit(20)
	}
	op, err := s.Get("settled")
	if err != nil {
		os.Exit(21)
	}
	phase := os.Getenv("TG_PAID_FEE_TEST_PHASE")
	if phase != "after-return" {
		s.rename = func(from, to string) error {
			if phase == "before-rename" {
				os.Exit(37)
			}
			if err := os.Rename(from, to); err != nil {
				return err
			}
			os.Exit(38)
			return nil
		}
	}
	if os.Getenv("TG_PAID_FEE_TEST_TRANSITION") == "record" {
		_, err = s.RecordOutgoingPaid(op.ID, op.Version)
	} else {
		_, err = s.ResolveOutgoingFee(op.ID, op.Version, 102)
	}
	if err != nil {
		os.Exit(22)
	}
	os.Exit(39)
}

func TestPaidFeePersistenceErrorsRefuseFurtherWork(t *testing.T) {
	for _, transition := range []string{"record", "resolve"} {
		for _, phase := range []string{"rename", "directory-sync"} {
			t.Run(transition+"/"+phase, func(t *testing.T) {
				s, dir := openTest(t)
				budget := Budget{LimitMSat: 2200}
				op, err := s.Prepare(intent("settled"), budget)
				if err != nil {
					t.Fatal(err)
				}
				op, err = s.SubmitOutgoing(op.ID, op.Version, budget)
				if err != nil {
					t.Fatal(err)
				}
				if transition == "resolve" {
					op, err = s.RecordOutgoingPaid(op.ID, op.Version)
					if err != nil {
						t.Fatal(err)
					}
				}
				fail := func() error { return errors.New("synthetic persistence failure") }
				if phase == "rename" {
					s.rename = func(string, string) error { return fail() }
				} else {
					s.syncDir = func(string) error { return fail() }
				}
				if transition == "record" {
					_, err = s.RecordOutgoingPaid(op.ID, op.Version)
				} else {
					_, err = s.ResolveOutgoingFee(op.ID, op.Version, 102)
				}
				if err == nil {
					t.Fatal("failed persistence was reported as success")
				}
				// Remove the injected fault: refusal must come from the poisoned
				// writer, not another encounter with the same failing seam.
				s.rename, s.syncDir = os.Rename, syncDirectory
				if _, err = s.Get(op.ID); err == nil {
					t.Fatal("writer remained readable after required I/O failure")
				}
				if _, err = s.Prepare(intent("must-stop"), Budget{LimitMSat: 1 << 20}); err == nil {
					t.Fatal("writer remained usable after required I/O failure")
				}
				if err = s.Close(); err != nil {
					t.Fatal(err)
				}
				again, err := Open(dir)
				if err != nil {
					t.Fatal(err)
				}
				defer again.Close()
				got, err := again.Get(op.ID)
				if err != nil {
					t.Fatal(err)
				}
				want, fee := PaidFeePending, uint64(0)
				if transition == "record" && phase == "rename" {
					want = Unknown
				} else if transition == "resolve" && phase == "directory-sync" {
					want, fee = Paid, 102
				}
				if got.State != want || got.ActualFeeMSat != fee {
					t.Fatalf("reopen after I/O refusal: state=%s fee=%d, want %s/%d", got.State, got.ActualFeeMSat, want, fee)
				}
			})
		}
	}
}

func TestPaidFeeMarkerHoldsConcurrentAdmissionAndCompetingResolution(t *testing.T) {
	s, _ := openTest(t)
	budget := Budget{LimitMSat: 1 << 20}
	a, err := s.Prepare(intent("settled"), budget)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Prepare(intent("prepared"), budget)
	if err != nil {
		t.Fatal(err)
	}
	a, err = s.SubmitOutgoing(a.ID, a.Version, budget)
	if err != nil {
		t.Fatal(err)
	}
	a, err = s.RecordOutgoingPaid(a.ID, a.Version)
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			future := Budget{LimitMSat: 1 << 30, Since: time.Now().Add(time.Hour)}
			if _, err := s.Prepare(intent(fmt.Sprintf("new-%d", i)), future); !errors.Is(err, ErrUnresolvedFee) {
				t.Errorf("concurrent prepare = %v", err)
			}
			if _, err := s.SubmitOutgoing(b.ID, b.Version, future); !errors.Is(err, ErrUnresolvedFee) {
				t.Errorf("concurrent prepared submit = %v", err)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	if got, err := s.Get(b.ID); err != nil || got.State != Prepared || got.Version != b.Version {
		t.Fatalf("held operation changed: %+v %v", got, err)
	}
	results := make(chan error, 2)
	for _, fee := range []uint64{0, 102} {
		wg.Add(1)
		go func(fee uint64) {
			defer wg.Done()
			_, err := s.ResolveOutgoingFee(a.ID, a.Version, fee)
			results <- err
		}(fee)
	}
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("competing resolutions succeeded %d times, want one", successes)
	}
	got, err := s.Get(a.ID)
	if err != nil || got.State != Paid || got.Version != a.Version+1 || (got.ActualFeeMSat != 0 && got.ActualFeeMSat != 102) {
		t.Fatalf("competing resolution result: %+v %v", got, err)
	}
}
