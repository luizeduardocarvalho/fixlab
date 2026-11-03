Finalise the refined spec. The current directory is the code at `{{.BaseCommit}}`.

The issue:

---
{{.Description | trimRight}}
---

The draft:

---
{{.Draft | trimRight}}
---

The maintainer's decisions:

---
{{.Decisions | trimRight}}
---

Start with: drafted by reading the code at {{printf "%.7s" .BaseCommit}}, read-only. End with `## Editorial notes` if you made editorial choices.
