// pair/web: an HTTP counter that keeps its count in the other unit of its own
// Package. The two share the pod's network namespace, so the cache is on
// localhost at the port it listens on and no address has to be discovered.
//
// The Redis protocol is spoken by hand rather than with a client library: a
// fixture that needs npm install is a fixture the end to end job has to wait
// for, and INCR is one command.
import { createServer } from "node:http";
import { connect } from "node:net";

const cachePort = Number(process.env.CACHE_PORT ?? 6379);

// incr sends one INCR and resolves with the number the cache answered. A new
// connection per request keeps this to the one thing it is for; the whole
// exchange is inside the pod.
function incr(key) {
  return new Promise((resolve, reject) => {
    const socket = connect({ host: "127.0.0.1", port: cachePort }, () => {
      socket.write(`*2\r\n$4\r\nINCR\r\n$${key.length}\r\n${key}\r\n`);
    });
    socket.setTimeout(5000, () => {
      socket.destroy();
      reject(new Error("the cache did not answer in time"));
    });
    socket.on("data", (chunk) => {
      const line = chunk.toString().trim();
      socket.end();
      if (!line.startsWith(":")) {
        reject(new Error(`the cache answered ${line}`));
        return;
      }
      resolve(Number(line.slice(1)));
    });
    socket.on("error", reject);
  });
}

const server = createServer(async (req, res) => {
  if (req.url === "/healthz") {
    res.writeHead(200, { "content-type": "application/json" });
    res.end(JSON.stringify({ ok: true }));
    return;
  }
  try {
    const count = await incr("kitbash:e2e:count");
    res.writeHead(200, { "content-type": "application/json" });
    res.end(JSON.stringify({ count, unit: process.env.KITBASH_UNIT ?? "" }));
  } catch (err) {
    res.writeHead(503, { "content-type": "application/json" });
    res.end(JSON.stringify({ error: String(err) }));
  }
});

server.listen(8080, "0.0.0.0", () => {
  console.log("pair/web listening on 8080, cache on 127.0.0.1:" + cachePort);
});
