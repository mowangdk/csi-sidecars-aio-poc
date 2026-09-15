CMDS=csi-sidecars snapshot-controller snapshot-conversion-webhook
all: build

include release-tools/build.make

# Project-owned recipe overrides; release-tools remains unchanged. GNU Make
# reports these intentional overrides when loading this file. Inherited
# prerequisites (including container/push -> build) remain connected.
# Explicit target required: make build BUILD_ARCH=amd64 (or arm64).
# The inherited release/push workflows are not a validated publication path.
.PHONY: $(CMDS:%=build-%)
$(CMDS:%=build-%): build-%:
	@test -z '$(BUILD_PLATFORMS)' || { echo 'Use BUILD_ARCH with a separate assembly per target'; exit 1; }
	@test '$(CMDS_DIR)' = cmd || { echo 'CMDS_DIR must remain cmd for locked Dockerfiles'; exit 1; }
	python3 -B tools/scripts/build_binaries.py build --command $* --arch '$(BUILD_ARCH)'

# Regenerate the assembly area (cmd/, pkg/, staging/, go.mod/go.work) from the
# upstream kubernetes-csi repositories. Requires Linux, see tools/scripts/sync.sh.
.PHONY: sync
sync:
	./tools/scripts/sync.sh

# Extend release-tools' `clean` (which only removes bin/) so it also removes
# the generated assembly area; cleanup.sh leaves tools/ untouched.
.PHONY: clean-generated
clean: clean-generated
clean-generated:
	./tools/scripts/cleanup.sh
