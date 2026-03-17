ROOT_DIR := $(abspath ../../..)
LOCAL_PREFIX ?= $(HOME)/.local
GO ?= $(shell command -v go)

# Keep local pkg-config metadata visible to cgo pkg-config checks.
PKG_CONFIG_PATH ?= $(LOCAL_PREFIX)/lib/pkgconfig

# Extra include/lib search paths for local headers and libraries.
# Keep libavpipe/include first so avpipe_xc.h resolves to the active libavpipe header.
CGO_CFLAGS_EXTRA ?= -I$(ROOT_DIR)/libavpipe/include -I$(ROOT_DIR)/utils/include -I$(ROOT_DIR)/include -I$(LOCAL_PREFIX)/include
CGO_LDFLAGS_EXTRA ?= -L$(ROOT_DIR)/lib -L$(LOCAL_PREFIX)/lib

export PKG_CONFIG_PATH := $(PKG_CONFIG_PATH)
export CGO_CFLAGS := $(CGO_CFLAGS_EXTRA) $(CGO_CFLAGS)
export CGO_CPPFLAGS := $(CGO_CFLAGS_EXTRA) $(CGO_CPPFLAGS)
export CGO_LDFLAGS := $(CGO_LDFLAGS_EXTRA) $(CGO_LDFLAGS)

BIN_DIR := bin
BIN := $(BIN_DIR)/stitch

.PHONY: help env build run clean build-override run-override

help:
	@echo "Targets:"
	@echo "  make env    - print effective CGO/pkg-config env"
	@echo "  make build  - build stitch binary"
	@echo "  make run    - run stitch main package"
	@echo "  make build-override PKG_CONFIG_PATH=... CGO_CFLAGS_EXTRA=... CGO_LDFLAGS_EXTRA=..."
	@echo "             - one-off build with explicit env overrides"
	@echo "  make run-override PKG_CONFIG_PATH=... CGO_CFLAGS_EXTRA=... CGO_LDFLAGS_EXTRA=..."
	@echo "             - one-off run with explicit env overrides"
	@echo "  make clean  - remove build artifacts"

env:
	@echo "ROOT_DIR=$(ROOT_DIR)"
	@echo "PKG_CONFIG_PATH=$(PKG_CONFIG_PATH)"
	@echo "CGO_CFLAGS=$(CGO_CFLAGS)"
	@echo "CGO_CPPFLAGS=$(CGO_CPPFLAGS)"
	@echo "CGO_LDFLAGS=$(CGO_LDFLAGS)"

build:
	@mkdir -p $(BIN_DIR)
	$(GO) build -o $(BIN) .

run:
	$(GO) run .

# These targets are aliases that make one-off overrides self-documenting.
# Example:
# make build-override \
#   PKG_CONFIG_PATH="$HOME/.local/lib/pkgconfig:/opt/ffmpeg/lib/pkgconfig" \
#   CGO_CFLAGS_EXTRA="-I$(ROOT_DIR)/include -I/opt/ffmpeg/include" \
#   CGO_LDFLAGS_EXTRA="-L$(ROOT_DIR)/lib -L/opt/ffmpeg/lib"
build-override: build

run-override: run

clean:
	@rm -rf $(BIN_DIR)
