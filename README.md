# predictsim-prediction-service

Go service: live game listing + prediction placement, plus a **background
scheduler** that generates rounds on a ticker. The scheduler needs an
always-on process — this service is **not** deployable to serverless/Vercel.
Host it on Railway / Render / Fly.io / a VM.

## Local dev
```bash
cp .env.example .env   # then fill DATABASE_URL (same DB as core-service)
go run ./cmd/server
```

## Env
See `.env.example`. `DATABASE_URL` must point at the same Supabase database
core-service migrates. `SESSION_COOKIE_NAME` must match core-service.
