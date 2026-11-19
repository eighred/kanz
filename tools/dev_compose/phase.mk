# Add a causal observation point without replacing any canonical recipe.
.PHONY: dev-proof-isolated
generate: | bootstrap
up: | dev-proof-isolated
dev-proof-isolated: generate
	python3 /work/repo/tools/dev_compose/gate.py
