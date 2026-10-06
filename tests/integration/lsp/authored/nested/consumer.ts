import settings from "../settings.json";
import { authoredValue as imported } from "#authored";
import { authoredValue as exported } from "authored-fixture/value";
import type { first } from "#types/first";
import type { z } from "zod";

export const setting: string = settings.value;
export const packageValues: string[] = [imported, exported];
export const generated: typeof first = "first";
export const npmValue: z.infer<z.ZodString> = generated;
