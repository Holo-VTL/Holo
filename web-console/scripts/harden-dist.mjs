import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import { readFile, writeFile } from "node:fs/promises";

const distDir = new URL("../dist/", import.meta.url);
const indexPath = new URL("index.html", distDir);
const csp =
  "default-src 'self'; script-src 'self' 'wasm-unsafe-eval'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; object-src 'none'; base-uri 'self'";

function sri(bytes) {
  return `sha384-${createHash("sha384").update(bytes).digest("base64")}`;
}

function assetPath(urlPath) {
  if (!urlPath.startsWith("/ui/assets/")) {
    return undefined;
  }
  return new URL(urlPath.replace(/^\/ui\//, ""), distDir);
}

function addAttribute(tag, name, value) {
  const existing = new RegExp(`\\s${name}(?:\\s|=|>)`);
  if (existing.test(tag)) {
    return tag;
  }
  return tag.replace(/>$/, ` ${name}="${value}">`);
}

function setAttribute(tag, name, value) {
  const existing = new RegExp(`\\s${name}="[^"]*"`);
  if (existing.test(tag)) {
    return tag.replace(existing, ` ${name}="${value}"`);
  }
  return addAttribute(tag, name, value);
}

function addCSPMeta(html) {
  if (html.includes('http-equiv="Content-Security-Policy"')) {
    return html;
  }
  const meta = `    <meta http-equiv="Content-Security-Policy" content="${csp}" />`;
  return html.replace(/(    <meta name="viewport"[^>]*>\n)/, `$1${meta}\n`);
}

async function main() {
  let html = await readFile(indexPath, "utf8");
  let assetCount = 0;

  html = html.replace(/<(script|link)\b[^>]*(?:src|href)="([^"]+)"[^>]*>/g, (tag, _kind, urlPath) => {
    const path = assetPath(urlPath);
    if (!path) {
      return tag;
    }
    assetCount += 1;
    const bytes = readFileSync(path);
    let hardened = setAttribute(tag, "integrity", sri(bytes));
    hardened = addAttribute(hardened, "crossorigin", "anonymous");
    return hardened;
  });

  if (assetCount === 0) {
    throw new Error("no /ui/assets entries found in dist/index.html");
  }

  html = addCSPMeta(html);
  await writeFile(indexPath, html, "utf8");
}

main().catch((err) => {
  console.error(err.message);
  process.exit(1);
});
