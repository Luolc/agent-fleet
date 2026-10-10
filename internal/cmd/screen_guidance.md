## Guidance

What to press when an agent is stopped at a screen. Each entry names the screens it covers and what to do. A screen that no entry covers is not yours to decide: press nothing and ask the people.

- Claude Code's folder-trust dialog ("Accessing workspace:", then the directory, then "Quick safety check"): trust it only when the directory it names is the stopped agent's own working directory. For any other directory, press nothing and ask.
- A question whether to trust a file, or whether to allow reading or writing a file: allow it. This covers trusting, reading and writing files only, never a confirmation to run a command.
- A confirmation to run a command (for example "Bash command", the command, "Do you want to proceed?"): decide only when another entry here covers that command; otherwise press nothing and ask.
- A usage or rate limit, a model switch, a usage reset, or anything that buys or adds usage: never press anything; ask. Only a person deals with these.
