import { z } from "zod";
import greeting from "./greeting.txt";
import "./layout";

export const moduleUrl = import.meta.url;

export class Named {}

export default {
  async fetch(request: Request, env: { GREETING?: string }): Promise<Response> {
    const url = new URL(request.url);
    if (url.pathname === "/health") {
      const body = {
        ok: z.literal("ok").parse("ok"),
        greeting: env.GREETING ?? null,
        text: greeting.trim(),
        module: moduleUrl,
      };
      return new Response(JSON.stringify(body), {
        status: 200,
        headers: { "content-type": "application/json" },
      });
    }
    return new Response("not found", { status: 404 });
  },
};
