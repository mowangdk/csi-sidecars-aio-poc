CMDS=csi-sidecars snapshot-controller snapshot-conversion-webhook
all: build

include release-tools/build.make

# The inherited release-tools build-% compiles each command with a plain
# `go build` (stamping main.version from REV). Builds use the vendor/ directory
# automatically when it is present, so a populated tree needs no sync first.

# Regenerate the assembly area (cmd/, pkg/, staging/, go.mod/go.work, vendor/)
# from the upstream kubernetes-csi repositories. Requires Linux and the locked
# builder; see tools/scripts/sync.sh.
.PHONY: sync
sync:
	@test -n '$(KUBERNETES)' || { echo 'Usage: make sync KUBERNETES=1.MINOR.PATCH'; exit 1; }
	./tools/scripts/sync.sh --update-dependencies '$(KUBERNETES)'

# Extend release-tools' `clean` (which only removes bin/) so it also removes
# the generated assembly area; cleanup.sh leaves tools/ untouched.
.PHONY: clean-generated
clean: clean-generated
clean-generated:
	./tools/scripts/cleanup.sh
