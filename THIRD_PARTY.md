# Third-party components

This deployment repository bundles modified source snapshots, not their Git history.

| Component | Upstream | Base revision | License |
| --- | --- | --- | --- |
| New API | https://github.com/QuantumNous/new-api | `633b4c5b190dc1af99a2814391354d86948e7943` | GNU AGPL-3.0; see `src/new-api/LICENSE`, `NOTICE`, `THIRD-PARTY-LICENSES.md` |
| copilot-api-proxy | https://github.com/messense/copilot-api-proxy | `6add6d57da6fc19cac525792c19887a96e1cf70b` | MIT; see `src/copilot-api-proxy/LICENSE` |

Upstream names, attribution, license notices and frontend branding are retained.
Packaging does not relicense either component. Deployers must satisfy the applicable
licenses, including the AGPL corresponding-source obligations for modified New API
when providing network access. A private GitHub repository alone does not satisfy
those obligations for users who cannot access it.

Local compatibility modifications include:

- New API: model-scoped removal of Argon built-in web search, image budget policy,
  narrowly scoped legacy Responses item-ID normalization, stable converted stream
  identities, and exact-origin request-body limits for the two proxy services.
- copilot-api-proxy: 32 MiB request-body cap, request/stream transport diagnostics,
  stable Responses SSE response/item IDs, and a 300-second idle-read timeout instead
  of a total-response timeout that cut off progressing streams.

`source-provenance.json` records SHA-256 hashes of the imported source snapshots
(CRLF normalized to LF for files without NUL bytes, so Git checkouts agree).
The deployment files are newly authored; upstream source modifications are carried
forward, not redesigned as part of this packaging task.
