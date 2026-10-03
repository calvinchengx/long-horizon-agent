import { defineConfig } from 'astro/config';
import starlight from '@astrojs/starlight';
import { remarkMermaid } from './plugins/remark-mermaid.mjs';

// Project GitHub Pages site: https://calvinchengx.github.io/long-horizon-agent/
// The docs live at the site root; the home page is src/landing/index.mdx (a Starlight splash page)
// and every chapter is generated from ../docs/NN-name.md by scripts/sync-docs.mjs.
export default defineConfig({
  site: 'https://calvinchengx.github.io',
  base: '/long-horizon-agent/',
  // remarkMermaid turns ```mermaid fences into <pre class="mermaid"> before Expressive Code sees
  // them; src/components/Head.astro renders them client-side.
  markdown: {
    remarkPlugins: [remarkMermaid],
  },
  integrations: [
    starlight({
      title: 'LHA',
      description:
        'Long-Horizon Agent: a durable agent organization that makes verified progress on software missions spanning days to weeks.',
      favicon: '/favicon.svg',
      components: {
        SiteTitle: './src/components/SiteTitle.astro',
        Head: './src/components/Head.astro',
      },
      social: [
        { icon: 'github', label: 'GitHub', href: 'https://github.com/calvinchengx/long-horizon-agent' },
      ],
      editLink: {
        baseUrl: 'https://github.com/calvinchengx/long-horizon-agent/edit/main/docs/',
      },
      sidebar: [
        {
          label: 'Getting started',
          items: [
            { slug: '01-introduction' },
            { slug: '02-quickstart' },
            { slug: '03-installation' },
            { slug: '04-choosing-an-implementation' },
          ],
        },
        {
          label: 'Tutorials',
          items: [{ slug: '26-tutorial-durable-mission' }],
        },
        {
          label: 'Concepts',
          items: [
            { slug: '05-architecture' },
            { slug: '06-mission-anchor' },
            { slug: '07-verification' },
            { slug: '08-durable-execution' },
            { slug: '09-safety-model' },
            { slug: '10-cost-and-budget' },
            { slug: '11-multi-agent-organization' },
            { slug: '12-memory' },
          ],
        },
        {
          label: 'Guides',
          items: [
            { slug: '13-models' },
            { slug: '14-running-on-temporal' },
            { slug: '15-operations-runbook' },
            { slug: '16-observability' },
            { slug: '24-large-missions' },
            { slug: '25-system-one' },
          ],
        },
        {
          label: 'Reference',
          items: [
            { slug: '17-cli' },
            { slug: '18-configuration' },
            { slug: '19-wire-contract' },
            { slug: '20-testing' },
          ],
        },
        {
          label: 'Project',
          items: [
            { slug: '21-contributing' },
            { slug: '22-honesty' },
            { slug: '23-roadmap' },
            { slug: '27-mission-ui' },
          ],
        },
      ],
    }),
  ],
});
