export async function leafIdentities(): Promise<unknown[]> {
  const emitted: string = "#leaf";
  const source: string = "#sourceLeaf";
  return [(await import(emitted)).identity, (await import(source)).identity];
}
