### Changed

- **Gazelle keeps fewer copies of what it scanned.** A cached source scan no
  longer holds the whole source text through its resolution counts, a scan's
  digest is taken once per source instead of once per listing that holds it,
  and the extension reads one kind catalog instead of rebuilding it per call.
  Generated BUILD files are unchanged.
