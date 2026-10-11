# Local Web Labs

Both fixtures bind only to `127.0.0.1` and use Python's standard library.

## Authorization lab

```powershell
python labs/web_authz_lab.py --port 8781
```

- `GET /api/resource/1` with `X-User-ID: 1`: expected `200`.
- `GET /api/resource/2` with `X-User-ID: 1`: intentionally vulnerable, expected `200`.
- The same request without `X-User-ID`: expected `403`.

## Template lab

```powershell
python labs/web_ssti_lab.py --port 8782
```

- `GET /greet?name=Trace`: expected `200` and `Hello, Trace`.
- `GET /greet?name={{7*7}}`: intentionally vulnerable, expected `Hello, 49`.
- `GET /greet?name=%7B%7B7%2B7%7D%7D`: expected `Hello, 14`.

Stop either process with `Ctrl+C`.
