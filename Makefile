# Release binaries for every platform a node can run on. The client uploads
# the matching one when it deploys a node over SSH (deploy.go looks in dist/).
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64
LDFLAGS := -s -w
# -trimpath keeps this machine's source paths out of the programs.
GOFLAGS_BUILD := -trimpath -ldflags "$(LDFLAGS)"

.PHONY: build test dist android macos clean

build:
	go build $(GOFLAGS_BUILD) -o rymesh ./cmd/rymesh

test:
	go vet ./... && go test -race ./...

dist:
	@mkdir -p dist
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; \
		echo "building $$os/$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build $(GOFLAGS_BUILD) -o dist/rymesh_$${os}_$${arch} ./cmd/rymesh || exit 1; \
	done
	@# The apps carry the Linux node programs gzipped and upload them that way.
	@for a in amd64 arm64; do gzip -9 -n -c dist/rymesh_linux_$$a > dist/rymesh_linux_$$a.gz; done

# Android app: the Go console as an AAR (gomobile) plus the Linux node
# programs it uploads when deploying to a server. Needs ANDROID_HOME,
# ANDROID_NDK_HOME and JAVA_HOME (Android Studio's JBR works). Phones only:
# no x86_64 library (emulators). A release build (not debuggable, shrunk by
# R8), signed with the debug key so it installs over earlier builds.
android: dist
	gomobile bind -target=android/arm64 -androidapi 26 -javapkg dev.rymesh \
		-trimpath -ldflags "$(LDFLAGS)" -o android/app/libs/rymesh.aar ./mobile
	rm -rf android/app/src/main/assets/dist
	mkdir -p android/app/src/main/assets/dist
	@# Not *.gz: the Android build would unpack those into the APK.
	for a in amd64 arm64; do cp dist/rymesh_linux_$$a.gz android/app/src/main/assets/dist/rymesh_linux_$$a.gzip; done
	cd android && ./gradlew assembleRelease

# macOS app: Swift shell + universal Go console, ad-hoc signed.
macos: dist
	./macos/build-app.sh

clean:
	rm -rf dist rymesh macos/dist macos/dist.noindex macos/.build android/app/build android/app/libs android/app/src/main/assets/dist
