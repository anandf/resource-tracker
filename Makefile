IMAGE_NAMESPACE?=quay.io/mangaal
IMAGE_NAME=argocd-resource-tracker
IMAGE_TAG?=latest
IMAGE_PUSH?=no
OS?=$(shell go env GOOS)
ARCH?=$(shell go env GOARCH)
OUTDIR?=dist
BINNAME?=argocd-resource-tracker

CURRENT_DIR=$(shell pwd)
VERSION=$(shell cat ${CURRENT_DIR}/VERSION 2>/dev/null || echo "dev")
GIT_COMMIT=$(shell git rev-parse --short HEAD)
BUILD_DATE=$(shell date -u +'%Y-%m-%dT%H:%M:%SZ')
LDFLAGS=-w -s -X main.Version=${VERSION} -X main.GitCommit=${GIT_COMMIT} -X main.BuildDate=${BUILD_DATE}

RELEASE_IMAGE_PLATFORMS?=linux/amd64,linux/arm64

ifeq ($(IMAGE_PUSH), yes)
DOCKERX_PUSH=--push
else
DOCKERX_PUSH=
endif

.PHONY: all
all: build

.PHONY: clean
clean:
	rm -rf vendor/ ${OUTDIR}/

.PHONY: mod-tidy
mod-tidy:
	go mod tidy

.PHONY: mod-download
mod-download:
	go mod download

.PHONY: build
build:
	mkdir -p ${OUTDIR}
	CGO_ENABLED=0 GOOS=${OS} GOARCH=${ARCH} go build -ldflags "${LDFLAGS}" -o ${OUTDIR}/${BINNAME} cmd/*.go

.PHONY: test
test:
	go test -coverprofile coverage.out `go list ./... | grep -vE '(test|mocks|vendor)'`

.PHONY: lint
lint:
	golangci-lint run

.PHONY: image
image: clean
	docker build -t ${IMAGE_NAMESPACE}/${IMAGE_NAME}:${IMAGE_TAG} --pull .

.PHONY: image-push
image-push: image
	docker push ${IMAGE_NAMESPACE}/${IMAGE_NAME}:${IMAGE_TAG}

.PHONY: multiarch-image
multiarch-image:
	docker buildx build -t ${IMAGE_NAMESPACE}/${IMAGE_NAME}:${IMAGE_TAG} --progress plain --pull --platform ${RELEASE_IMAGE_PLATFORMS} ${DOCKERX_PUSH} .

.PHONY: release-binaries
release-binaries:
	BINNAME=argocd-resource-tracker-linux_amd64 OUTDIR=${OUTDIR}/release OS=linux ARCH=amd64 make build
	BINNAME=argocd-resource-tracker-linux_arm64 OUTDIR=${OUTDIR}/release OS=linux ARCH=arm64 make build
	BINNAME=argocd-resource-tracker-darwin_amd64 OUTDIR=${OUTDIR}/release OS=darwin ARCH=amd64 make build
	BINNAME=argocd-resource-tracker-darwin_arm64 OUTDIR=${OUTDIR}/release OS=darwin ARCH=arm64 make build
	BINNAME=argocd-resource-tracker-windows_amd64.exe OUTDIR=${OUTDIR}/release OS=windows ARCH=amd64 make build