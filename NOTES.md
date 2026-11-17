# Notes

I used Claude for most of the work, as one orchestrating session with sub-agents. The session planned the tasks and wrote the shared groundwork, sub-agents wrote the larger pieces (start and its store rules, the worker, the brochure rules, the page, the Docker files), and the session reviewed each of their diffs under one rule from me: take no report on trust, read the diff and run the tests yourself.

Three of Claude's suggestions did not survive review.

**A race test that could not fail.** A sub-agent wrote the test for simultaneous `POST /start` requests on one sheet (16 goroutines against the in-memory store) and reported it green. A green concurrency test proves nothing until it has been seen to fail, so the per-account lock was removed from the handler in a scratch copy and the test was run 40 times. It failed 10 times under the race detector and not once without it. The test was rejected as written. It now runs on a store that pauses for 2 ms after every read, so two requests that decide on the same read always collide: without the lock it fails 40 runs out of 40, with and without `-race`, and with the lock it passes.

**A page that refreshed only while it knew of a running sheet.** Claude's version of the page fetched the list only while the tab itself held a running sheet. A final review by a session that had seen none of the reasoning questioned it, and a check in the browser confirmed it: a sheet was started from outside the page with a direct `POST /start`, as a second tab would, and seven seconds later the server said `success` while the page still showed a draft. The page now also refreshes every three seconds when nothing is running; in the same check it caught up within three seconds and reached `success` without a reload.

**A file name limit counted in bytes.** Claude proposed refusing brochure names longer than 255 bytes. The same final review objected that the README has no such rule. A 130-character Cyrillic name is 260 bytes, so an ordinary file would have been refused. The limit is now 255 characters, and a test uploads a 255-character, 506-byte name; putting the byte limit back makes that test fail.

The other rules were checked the same way, by breaking each one on purpose in a copy of the repository and confirming that a test fails: the owner check, the 1 MiB limit, the type decided from bytes, one running sheet per account, the migration lock, the worker's pause and its brochure check. Then the stack ran in Docker and the worker was killed in the middle of a run: the sheet stayed `running` with the same `runId`, and the restarted worker finished it without repeating a step.

The store tests need Postgres and are skipped by `go test ./...`; `make test-store` runs them against the database from `docker compose`.
