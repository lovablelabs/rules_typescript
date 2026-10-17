### Fixed

- **`-ts_listing_patch_verify` no longer aborts a run on a listing nothing
  took, and `-ts_listing_inproc=verify` no longer caches a listing it did not
  check.** The walk's end cancels the speculative and prefetched listings no
  directory asked for, and the patch check failed the whole run with
  `tsconfig.json: context canceled` when one of them was still verifying a
  patched or narrowed listing; the in-process check skipped any cancelled
  comparison and cached the listing anyway. Both checks now drop a listing
  nothing took without caching it, and fail the run on any other
  cancellation, a tsgo failure or a listing that differs.
