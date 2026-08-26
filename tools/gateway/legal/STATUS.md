# oaica Inference API — Service Status & Availability Statement

Live health: GET https://api.oaica.com/health (200 = at least one backend serving; 503 = down)

What we run
- One inference cluster on a single rented GPU server in Japan (JP);
  operator headquartered in Malaysia.
- Models: kat-awq (text, tool calling, 262k context).

What to expect
- No SLA. Best-effort availability.
- The service can be fully unavailable if the single host is rebooted,
  reclaimed by the hosting marketplace, or loses its model weights
  (they live on volatile storage and must be re-downloaded after loss).
- Under load you will receive HTTP 429 (per-key concurrency) or 503
  ("no healthy backend"). Retry with backoff.
- Maintenance is unscheduled; we will post incidents on the status page
  above and notify OpenRouter for outages longer than 30 minutes.

Health endpoint (unauthenticated, for monitors)
- GET https://api.oaica.com/health -> 200 when the upstream answers a
  real request, 503 otherwise. Body: {"status":"ok"} or {"status":"down","reason":...}.

Incident contact: oaica@sprapp.com
