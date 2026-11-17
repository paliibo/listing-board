.PHONY: test test-store up

test:
	go test ./...

# The store tests need a real Postgres and are skipped without one. This runs
# them against the database docker compose publishes on localhost:54329; each
# test works in a database of its own and drops it afterwards.
test-store:
	TEST_DATABASE_URL="postgres://board:board@localhost:54329/board?sslmode=disable" go test -count=1 ./internal/store/

up:
	docker compose up --build
