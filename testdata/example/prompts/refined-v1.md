You are working in the example {{if eq .Repo "web"}}web app{{else}}API{{end}} repository (the current directory). Implement the change described in this issue and its refined spec:

---
{{.Description}}
---

Refined spec:

---
{{.Refined | trimRight}}
---

Do not commit; leave your changes in the working tree.
