### Changed

- **Gazelle completes programs in a fixed order and answers each owner's
  emission from the plan.** After Resolve, programs completed in Go's random map
  order, and a completion promoted the owners it reached while it was still
  asking them for their emission mode. Programs now complete in label order,
  an owner that a rooted program reaches reads as emitted when the pass
  would promote it, and the pass visits what a completion reached after it
  returns. BUILD output is unchanged.
