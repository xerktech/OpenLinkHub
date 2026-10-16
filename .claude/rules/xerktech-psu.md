---
paths:
  - "src/devices/psuhid/**"
  - "src/metrics/**"
  - "src/monitor/**"
---
# xerktech fork: PSU telemetry on talos04

- Fork changes live on branch `xerktech-0.9.2` (tags `0.9.2-xerk.N` build the image); `main` tracks upstream.
- The pod is not `hostNetwork`, so `src/monitor` receives no USB uevents: hotplug never fires there.
  - A PSU that re-enumerates must recover inside `psuhid` itself (`reopen`), not via monitor/StopDirty.
- Logs go to `/opt/OpenLinkHub/stdout.log` in the container, not pod stdout; it is not persistent
  and is archived on restart, so `kubectl logs` shows nothing for device errors.
- Never publish a zeroed buffer as a reading: a failed transfer returns 64 zero bytes, which decode
  to 0 W. Drop the series (`metrics.RemovePsu`) instead so absent()-style alerts fire.
- `hid.ErrTimeout` is an unanswered request, not a dead handle: skip that refresh, keep the last values;
  after `maxFailedRefreshes` failed refreshes in a row drop the series so a hung PSU is not shown live.
  - Only other read/write errors mark the handle lost.
- Serialise every `hid.Enumerate`/`hid.OpenPath` (`hidMutex`): hidapi's global error string races (double free).
- Fail-fast after a failed transfer belongs only to the refresh (`refreshTransfer`), never `transfer`:
  a shared flag there silently dropped API fan writes, the reopen fan restore and Stop's fan reset.
