- Outgoing Lightning journal settlements now persist `paid_fee_pending` before
  authenticated fee resolution. The wallet's outgoing admission stays held across
  restarts and budget-period changes until the full verified fee is recorded;
  explicit zero is supported, and generic transitions cannot bypass the hold.
  This does not enable a provider or qualify live payment integration.
  ([#1231](https://github.com/OpenTollGate/tollgate-module-basic-go/pull/1231))
