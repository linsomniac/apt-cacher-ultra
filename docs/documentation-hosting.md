# Documentation publishing options

The [configuration reference](configuration.md) lives beside the code so a
configuration change and its documentation can be reviewed together. GitHub
already renders it, and selecting a release tag shows that release's docs.
No documentation service or build step is required for this arrangement.

## Options without a monthly subscription

| Option | Benefits | Maintenance and tradeoffs |
| --- | --- | --- |
| Markdown in this repository | Available now; versioned with code; links from README; no extra infrastructure. | Basic navigation and repository search rather than a dedicated documentation site. |
| Docs alongside the apt repository on GitHub Pages | One project URL; existing deployment; add generated HTML under `/docs/` while preserving apt paths. | Docs and apt must be assembled into one site artifact on every deployment. |
| A separate public docs repository with its own Pages site | Independent deploys and URL, with no risk of a docs-only deploy replacing the apt site. | Another repository/workflow; render docs from a pinned source commit to avoid maintaining two copies. |
| Static files on the existing public server | Independent deployment, full control over URLs, and no additional hosting subscription on the existing server. | Maintain HTTPS, server updates, backups and deployment access; transfer generated files into a dedicated document root. |
| GitHub wiki | Built-in browser editing and simple navigation. | Separate Git history makes code/doc changes harder to keep together; less suitable for the authoritative config reference. |

GitHub Pages and wikis are available for public repositories on GitHub Free.
Pages serves static files and permits one project site per repository, with
as many paths within that site as needed. See [GitHub Pages](https://docs.github.com/en/pages/getting-started-with-github-pages/what-is-github-pages)
and [GitHub wikis](https://docs.github.com/en/communities/documenting-your-project-with-wikis/about-wikis).

## Recommended next step: one Pages site for docs and apt

Keep Markdown as the source and add a static HTML build to the existing
[Pages workflow](../.github/workflows/pages.yaml). A Markdown static-site
generator such as Jekyll or Hugo can provide navigation and styling; choose
one only when the site needs those features. The same generated files can
also be copied to the public server, so the documentation source is portable.

Proposed deployment layout (beneath `/apt-cacher-ultra/`):

```text
index.html                 existing apt installation page, with a docs link
apt-cacher-ultra.gpg        existing public signing key
dists/                     existing signed apt metadata
pool/                      existing .deb packages
docs/index.html            documentation landing page
docs/configuration/        rendered configuration reference
```

The existing apt build already writes `site/`, and the workflow uploads that
directory. Add the docs to `site/docs/` after building the apt repository and
before the single upload/deploy. Preserve `dists/`, `pool/`, and the key at
their existing paths so installed apt clients need no changes. Treat every
deployment as a complete replacement: a second workflow publishing only
docs would replace the apt files. GitHub's [custom workflow documentation](https://docs.github.com/en/pages/getting-started-with-github-pages/using-custom-workflows-with-github-pages)
describes packaging and deploying the site artifact.

Implementation checklist for that future change:

1. Render only the intended public docs, with the project base path
   `/apt-cacher-ultra/docs/`. Include a downloadable example config and
   links back to the README and apt installation page. Copy required assets;
   do not expose the whole working directory as a site.
2. Combine the docs output and signed apt repository into one artifact.
   Keep the current signed-install smoke test and add rendered-link and
   required-file checks before upload.
3. Keep one deployment owner and the existing `pages` concurrency group.
   Build previews in pull requests without publishing or accessing the apt
   signing key. Only trusted branch/release builds should publish.
4. Choose an update policy: the current workflow runs after a successful
   release or on manual dispatch. A docs-only merge currently needs a manual
   rebuild. If adding a push trigger for docs, update the job condition too,
   and make every push deployment rebuild both parts of the site.
5. Label which release/commit the reference describes. Start with one current
   reference and links to tagged Markdown; add versioned HTML only if needed.

The apt packages and docs share the site's limits. GitHub currently specifies
a 1 GB published-site limit and a soft 100 GB/month bandwidth limit; package
downloads will dominate both. See [Pages limits](https://docs.github.com/en/pages/getting-started-with-github-pages/github-pages-limits).
If those become restrictive, the existing public server is a straightforward
destination for the same static output.

This change supplies the reference and this publishing proposal; it does not
alter the current apt publishing workflow or deploy a documentation site.
