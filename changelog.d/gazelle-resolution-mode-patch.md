### Fixed

- **A source that names a `resolution-mode` is listed again rather than
  patched.** tsgo honours the attribute on a type-only import or export, an
  import type and a `/// <reference types>` directive, and drops it beside any
  other attribute, so the patch could not tell an edit of it, or of the `type`
  beside it, from a body edit: the listing kept the old package entry point and
  the BUILD file differed from a full pass. Such a source no longer vouches for
  a patched edit, a new file naming one is listed by tsgo, and no new importer
  copies its resolution of a package. A key spelled with string escapes counts.
  Cached listings from before are listed once again.
