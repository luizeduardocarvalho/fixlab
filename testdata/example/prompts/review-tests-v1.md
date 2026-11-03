Review these acceptance tests against the code at `{{.BaseCommit}}`.

The spec:

---
{{.Refined | trimRight}}
---

Top-level tests are named `{{.TestPrefix}}…` and helpers start with `{{.HelperPrefix}}`.

```go
{{.Tests | trimRight}}
```

How each test did on the unfixed code:

```
{{.RedCheck | trimRight}}
```
