// Generates Starlight content from the canonical Markdown in /docs, keeping /docs as the single
// source of truth: its files stay plain GitHub Markdown and their relative links keep working on
// GitHub. Run automatically before `pnpm dev` and `pnpm build`.
//
// For each docs/NN-name.md it: takes the title from the leading H1, injects Starlight frontmatter
// (title + an "Edit this page" link to the real source), drops the duplicate H1, rewrites
// `NN-name.md` links to site routes, and rewrites repo-relative links (`../python/...`) to GitHub
// URLs, because on the site nothing sits above the docs. The home page is src/landing/index.mdx.
import { readdirSync, readFileSync, writeFileSync, rmSync, mkdirSync, existsSync, statSync, copyFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const here = dirname(fileURLToPath(import.meta.url));
const REPO = join(here, '..', '..');
const DOCS_SRC = join(REPO, 'docs');
const OUT = join(here, '..', 'src', 'content', 'docs');
const LANDING = join(here, '..', 'src', 'landing', 'index.mdx');
const BASE = '/long-horizon-agent/';
const REPO_URL = 'https://github.com/calvinchengx/long-horizon-agent';

const DOC_RE = /^\d{2}-[a-z0-9-]+\.md$/;
// `](NN-slug.md#anchor)` or `](./NN-slug.md)` -> `](/long-horizon-agent/NN-slug/#anchor)`.
const LINK_RE = /\]\((?:\.\/)?(\d{2}-[a-z0-9-]+)\.md(#[^)]*)?\)/g;
// `](../path#anchor)` -> an absolute GitHub URL (tree for directories, blob for files).
const REPO_LINK_RE = /\]\(\.\.\/([^)#\s]+)(#[^)]*)?\)/g;
// `](predicted-runs/x.md)` and other in-docs, non-chapter files -> GitHub URLs too.
const DOCS_FILE_RE = /\]\((?!https?:|#|\/|\.\.\/|images\/|\d{2}-[a-z0-9-]+\.md)([^)#\s]+)(#[^)]*)?\)/g;
// Images under docs/images/ (Markdown `](images/x.png)`, or `src`/`srcset` in a <picture>) are
// copied to public/images/ and served by the site itself.
const IMAGE_RE = /(\]\(|src="|srcset=")images\//g;
const IMAGES_SRC = join(DOCS_SRC, 'images');
const IMAGES_OUT = join(here, '..', 'public', 'images');

let warnings = 0;

function githubUrl(repoPath, anchor, where) {
  const clean = repoPath.replace(/\/+$/, '');
  const target = join(REPO, clean);
  const exists = existsSync(target);
  if (!exists) {
    console.warn(`sync-docs: WARNING ${where}: ${clean} matches nothing in the repo`);
    warnings++;
  }
  const kind = exists && statSync(target).isDirectory() ? 'tree' : 'blob';
  return `${REPO_URL}/${kind}/main/${clean}${anchor ?? ''}`;
}

function rewriteLinks(md, where) {
  return md
    .replace(LINK_RE, (_m, slug, anchor) => {
      if (!existsSync(join(DOCS_SRC, `${slug}.md`))) {
        console.warn(`sync-docs: WARNING ${where}: link to missing page ${slug}.md`);
        warnings++;
      }
      return `](${BASE}${slug}/${anchor ?? ''})`;
    })
    .replace(IMAGE_RE, (_m, lead) => `${lead}${BASE}images/`)
    .replace(REPO_LINK_RE, (_m, path, anchor) => `](${githubUrl(path, anchor, where)})`)
    .replace(DOCS_FILE_RE, (_m, path, anchor) => `](${githubUrl(`docs/${path}`, anchor, where)})`);
}

function yamlEscape(s) {
  return '"' + s.replace(/\\/g, '\\\\').replace(/"/g, '\\"') + '"';
}

function convert(name) {
  const raw = readFileSync(join(DOCS_SRC, name), 'utf8');
  const lines = raw.split('\n');
  const h1Index = lines.findIndex((l) => /^#\s+/.test(l));
  const title = h1Index >= 0 ? lines[h1Index].replace(/^#\s+/, '').trim() : name.replace(/\.md$/, '');
  if (h1Index >= 0) lines.splice(h1Index, lines[h1Index + 1]?.trim() === '' ? 2 : 1);
  const body = rewriteLinks(lines.join('\n').replace(/^\n+/, ''), name);
  const editUrl = `${REPO_URL}/edit/main/docs/${name}`;
  return `---\ntitle: ${yamlEscape(title)}\neditUrl: ${yamlEscape(editUrl)}\n---\n\n${body}`;
}

rmSync(OUT, { recursive: true, force: true });
mkdirSync(OUT, { recursive: true });
const names = readdirSync(DOCS_SRC).filter((n) => DOC_RE.test(n)).sort();
for (const name of names) writeFileSync(join(OUT, name), convert(name));
copyFileSync(LANDING, join(OUT, 'index.mdx'));
rmSync(IMAGES_OUT, { recursive: true, force: true });
if (existsSync(IMAGES_SRC)) {
  mkdirSync(IMAGES_OUT, { recursive: true });
  for (const image of readdirSync(IMAGES_SRC)) copyFileSync(join(IMAGES_SRC, image), join(IMAGES_OUT, image));
}
console.log(`sync-docs: wrote ${names.length} docs + the landing page to src/content/docs/`);
if (warnings && process.env.SYNC_DOCS_STRICT === '1') {
  console.error(`sync-docs: ${warnings} broken link(s) (SYNC_DOCS_STRICT=1)`);
  process.exit(1);
}
