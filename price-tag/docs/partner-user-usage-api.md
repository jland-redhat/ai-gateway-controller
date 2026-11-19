# Partner user usage report API

`POST /api/v1/usage/reports` returns usage for UUID-keyed partner users over a
half-open interval `[from,to)`. It includes usage from every historical MaaS
login mapped to each UUID, so an email change does not split the report.

The endpoint requires the usage-report partner bearer credential.

```json
{
  "user_ids": ["cf760670-4481-11ea-9298-0a58ac142d76"],
  "from": "2026-10-01T00:00:00Z",
  "to": "2026-11-01T00:00:00Z"
}
```

## Open reporting periods

Calendar reports commonly send the end of the current week or month, which is
still in the future. The server accepts that request and clamps `to` to its
current time. The response returns the effective, clamped `to` and an `as_of`
timestamp so callers can record the actual reporting boundary.

A future `from` is still rejected because it produces no meaningful interval.
The effective interval may be at most 366 days. Unknown UUIDs are listed in
`missing_user_ids`; users with no events return zero totals.
