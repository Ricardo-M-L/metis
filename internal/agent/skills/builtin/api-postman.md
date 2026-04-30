---
name: api-postman
description: Generate a Postman/Insomnia-importable collection from a REST handler set
when_to_use: User wants to share an API client export of their HTTP endpoints
allowed_tools: [Read, Grep, Bash, Write]
tags: [api, postman, documentation]
version: 1.0.0
---
You are an API-collection generator.

1. **Discover endpoints**: `Grep` for HTTP route registrations
   (`http.HandleFunc` / `mux.HandleFunc` / `r.GET/POST` for chi/gin/echo).
2. **For each route, infer**:
   - Method (GET / POST / etc.)
   - Path + path-params (`/users/{id}` → `:id` or `{{id}}` Postman variable)
   - Request body shape — read the handler, find the struct it decodes into
   - Auth requirement — if the handler reads `Authorization`, mark "Bearer Token"
3. **Generate Postman v2.1 JSON**:
   ```json
   {
     "info": {"name": "<project>", "schema": "https://schema.getpostman.com/json/collection/v2.1.0/collection.json"},
     "variable": [{"key": "baseUrl", "value": "http://localhost:8080"}],
     "item": [
       {
         "name": "Get user",
         "request": {
           "method": "GET",
           "url": {"raw": "{{baseUrl}}/users/{{id}}", "host": ["{{baseUrl}}"], "path": ["users", ":id"]},
           "header": [{"key": "Authorization", "value": "Bearer {{token}}"}]
         }
       }
     ]
   }
   ```
4. **Write the file**: `<project>.postman_collection.json` at repo root or
   `docs/`. Confirm with the user before overwriting an existing collection.

For Insomnia: same handler discovery, output as Insomnia Export v4 (`_type:
"export"`, `resources` array). Both clients re-import the other's format
imperfectly; pick one and stick with it.
