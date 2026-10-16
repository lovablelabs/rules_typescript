### Fixed

- **A changed JSON file the listing patch cannot vouch for is listed again.**
  The patch took any changed JSON module for a content edit, so an `include`
  or compiler option edited in a JSON a program both imports and extends as its
  tsconfig kept the old listing. Listing entries now record their config chain,
  and one whose chain extends a package's config vouches for no JSON. A run
  under `-ts_verbose` lists a changed JSON module again so that it logs its
  diagnostics, as it does a listing a run without it patched for a JSON edit.
  The patch state version moves to 7, and listing cache entries take a new
  layout: the first run after the upgrade lists every program once more.
