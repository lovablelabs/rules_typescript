import { store } from "#scope-store";

const selected: "A" = store;
if (selected !== "A") throw new Error(`package import selected store ${selected}`);
console.log(`package import retained scope store ${selected}`);
