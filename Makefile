CMDS=csi-sidecars snapshot-controller snapshot-conversion-webhook
all: build

include release-tools/build.make
