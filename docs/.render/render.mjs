import { chromium } from 'playwright-core';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import http from 'node:http';
import path from 'node:path';

const EXE = '/Users/shuhao/Library/Caches/ms-playwright/chromium-1228/chrome-mac-arm64/Google Chrome for Testing.app/Contents/MacOS/Google Chrome for Testing';
const here = path.dirname(fileURLToPath(import.meta.url));
const distDir = path.join(here, 'node_modules/mermaid/dist');
const htmlFile = path.join(here, '..', 'bean-architecture.html');

// Serve the render page + node_modules/mermaid/dist from one origin so the ESM
// import (and its lazy chunks) resolve without CORS/file:// restrictions.
let pageHtml = '';
const server = http.createServer((req, res) => {
  const url = decodeURIComponent(req.url.split('?')[0]);
  if (url === '/' || url === '/index.html') {
    res.writeHead(200, { 'content-type': 'text/html; charset=utf-8' });
    res.end(pageHtml);
    return;
  }
  const rel = url.replace(/^\/+/, '');
  try {
    const body = readFileSync(path.join(distDir, rel));
    res.writeHead(200, { 'content-type': 'text/javascript; charset=utf-8', 'access-control-allow-origin': '*' });
    res.end(body);
  } catch { res.writeHead(404); res.end('not found'); }
});
await new Promise(r => server.listen(0, r));
const port = server.address().port;
const mermaidUrl = `http://localhost:${port}/mermaid.esm.min.mjs`;

// Pull the mermaid source blocks out of the standalone HTML.
const html = readFileSync(htmlFile, 'utf8');
const blocks = [...html.matchAll(/<pre class="mermaid">([\s\S]*?)<\/pre>/g)].map(m => m[1].trim());
console.log('found', blocks.length, 'mermaid blocks');

pageHtml = `<!doctype html><html><head><meta charset="utf-8"></head>
<body style="background:#fff;margin:0;padding:24px;font-family:sans-serif">
${blocks.map((b,i)=>`<div class="wrap" style="margin:0 0 32px"><h3>Diagram ${i+1}</h3><pre class="mermaid">${b}</pre></div>`).join('\n')}
<script type="module">
import mermaid from ${JSON.stringify(mermaidUrl)};
mermaid.initialize({ startOnLoad:false, look:'handDrawn', theme:'neutral', flowchart:{curve:'basis'} });
try {
  await mermaid.run();
  window.__done = true;
} catch(e){ window.__err = String(e); window.__done = true; }
</script></body></html>`;

const browser = await chromium.launch({ executablePath: EXE });
const p = await browser.newPage({ viewport: { width: 1400, height: 2000 }, deviceScaleFactor: 2 });
const errors = [];
p.on('console', m => { if (m.type()==='error') errors.push(m.text()); });
p.on('pageerror', e => errors.push(String(e)));
await p.goto(`http://localhost:${port}/`, { waitUntil: 'networkidle' });
await p.waitForFunction('window.__done === true', { timeout: 20000 });

const diag = await p.evaluate(() => {
  const svgs = [...document.querySelectorAll('svg')];
  const errSvgs = [...document.querySelectorAll('svg[aria-roledescription="error"], .error, text.error-text')];
  const labels = [...document.querySelectorAll('foreignObject')].map(f => (f.textContent||'').trim()).filter(Boolean);
  return {
    svgCount: svgs.length,
    errSvgCount: errSvgs.length,
    pageErr: window.__err || null,
    labelCount: labels.length,
    labels,
  };
});
console.log('svgCount', diag.svgCount, 'errSvgCount', diag.errSvgCount, 'pageErr', diag.pageErr);
console.log('consoleErrors', errors.length ? errors.slice(0,5) : 'none');

// Assertions: the labels the user cares about must be present.
const joined = diag.labels.join(' | ');
const must = ['overlaybd','TCMU','/dev/sdX','UFFD','ResumeVM','snapshot','configureAndBoot','loadSnapshot','InstanceStart'];
const missing = must.filter(k => !joined.includes(k));
console.log('missing responsibility labels:', missing.length ? missing : 'none');

await p.screenshot({ path: path.join(here, '..', 'preview-all.png'), fullPage: true });
console.log('wrote preview-all.png');

await browser.close();
server.close();
const ok = diag.svgCount === blocks.length && diag.errSvgCount === 0 && !diag.pageErr && missing.length === 0;
console.log(ok ? 'RENDER OK' : 'RENDER FAILED');
process.exit(ok ? 0 : 1);
