import { value } from "../components/value.js";

if (value !== 37) throw new Error(`The emitted dependency returned ${value}`);
