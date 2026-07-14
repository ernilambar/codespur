// Test stub for an OpenAI-compatible /chat/completions endpoint.
// Behaviour keys off the model name so we can exercise each code path:
//   model contains "nostream" → returns a plain JSON (non-streaming) body
//   model contains "hang"     → never responds (tests idle --timeout)
//   otherwise                 → streams SSE word-by-word
// Severity is "high" for files whose name contains "app", else "none".
const server = Bun.serve({
  port: 1234,
  async fetch(req) {
    const url = new URL(req.url);
    if (!url.pathname.endsWith("/chat/completions")) {
      return new Response("not found", { status: 404 });
    }
    const body = await req.json();
    const model: string = body.model ?? "";
    const file = body.messages?.[1]?.content?.match(/File: (.*)/)?.[1] ?? "unknown";
    const sev = file.includes("app") ? "high" : "none";

    if (model.includes("hang")) {
      await Bun.sleep(60_000); // force the client's idle timeout to fire
      return new Response("late", { status: 200 });
    }

    const text =
      `Review of ${file}: logic looks changed; check edge cases and error handling. ` +
      `SEVERITY: ${sev}`;

    if (model.includes("nostream")) {
      return Response.json({ choices: [{ message: { role: "assistant", content: text } }] });
    }

    const words = text.split(" ");
    const stream = new ReadableStream({
      async start(c) {
        const enc = new TextEncoder();
        for (const w of words) {
          c.enqueue(enc.encode(`data: ${JSON.stringify({ choices: [{ delta: { content: w + " " } }] })}\n\n`));
          await Bun.sleep(3);
        }
        c.enqueue(enc.encode("data: [DONE]\n\n"));
        c.close();
      },
    });
    return new Response(stream, { headers: { "Content-Type": "text/event-stream" } });
  },
});
console.log("mock up on " + server.port);
