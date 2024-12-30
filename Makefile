include .env

.PHONY: run
run:
	go run . $(WALLET)
