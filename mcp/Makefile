.PHONY: build fixture integration-fixture integration-headless test check clean

TMPDIR ?= $(CURDIR)/.tmp
# The emulator is built from the repository root: make -C .. DEBUG=1
BLASTEM_BINARY ?= $(abspath $(CURDIR)/../blastem)

build:
	mkdir -p $(TMPDIR)
	TMPDIR=$(TMPDIR) go build -o blastem-mcp ./cmd/blastem-mcp

fixture:
	$(MAKE) -C testdata/fixture check

integration-fixture: fixture
	test -x "$(BLASTEM_BINARY)"
	BLASTEM_INTEGRATION_BINARY="$(BLASTEM_BINARY)" \
	BLASTEM_INTEGRATION_ROM="$(CURDIR)/testdata/fixture/build/blastem-mcp-fixture.bin" \
	BLASTEM_FIXTURE_ROM="$(CURDIR)/testdata/fixture/build/blastem-mcp-fixture.bin" \
	BLASTEM_FIXTURE_SYMBOLS="$(CURDIR)/testdata/fixture/build/symbols.txt" \
	TMPDIR="$(TMPDIR)" \
	go test -count=1 -run 'Test(MCPBlastEMIntegration|FixtureMCPContract|FrameMCPContract)$$' -v ./internal/mcpserver
	BLASTEM_INTEGRATION_BINARY="$(BLASTEM_BINARY)" \
	BLASTEM_INTEGRATION_ROM="$(CURDIR)/testdata/fixture/build/blastem-mcp-fixture.bin" \
	TMPDIR="$(TMPDIR)" \
	go test -count=1 -run 'TestBlastEM(Control|GDB|SaveProfile|SaveFailureOnStop)Integration$$' -v ./internal/session

# The same acceptance tests without a display server (SDL dummy drivers).
integration-headless:
	env -u DISPLAY -u WAYLAND_DISPLAY BLASTEM_TEST_HEADLESS=1 $(MAKE) integration-fixture

test:
	mkdir -p $(TMPDIR)
	TMPDIR=$(TMPDIR) go test ./...

check:
	mkdir -p $(TMPDIR)
	test -z "$$(gofmt -l cmd internal)"
	TMPDIR=$(TMPDIR) go vet ./...
	TMPDIR=$(TMPDIR) go test -race ./...

clean:
	$(RM) blastem-mcp
	$(MAKE) -C testdata/fixture clean
