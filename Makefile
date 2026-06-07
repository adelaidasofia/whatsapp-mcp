# Makefile — local gates for whatsapp-mcp.
#   make shellcheck   # shellcheck static analysis over tracked *.sh (same gate as CI)
.PHONY: help shellcheck

help:
	@echo "make shellcheck   # shellcheck static analysis over tracked *.sh (same gate as CI)"

# Shell static analysis over every tracked *.sh — the SAME canonical gate as
# .github/workflows/shellcheck.yml. scripts/shellcheck.sh is the single source of
# truth, so this local run and CI cannot drift. -S warning; info/style excluded.
shellcheck:
	bash scripts/shellcheck.sh
