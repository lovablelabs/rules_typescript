### Changed

- **The listing cache reads and writes its entries without gob.** An entry
  names each string once and then by number, is encoded and decoded as a
  stream instead of through a whole-message buffer, and a fresh hit becomes a
  program once instead of twice. Decoded strings are shared between entries,
  and the fast path still decodes only an entry's leading fields before it
  declines. Generated BUILD files are unchanged; entries written before the
  change miss once, and a corrupt or truncated entry is a miss.
