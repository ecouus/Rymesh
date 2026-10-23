# Release binaries for every platform a node can run on. The client uploads
# the matching one when it deploys a node over SSH (deploy.go looks in dist/).
# The version of every program is the Go console's (internal/util/util.go).
VERSION := $(shell sed -n 's/^const Version = "\(.*\)"/\1/p' internal/util/util.go)
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64
LDFLAGS := -s -w
# -trimpath keeps this machine's source paths out of the programs.
GOFLAGS_BUILD := -trimpath -ldflags "$(LDFLAGS)"

.PHONY: build test dist android macos release clean

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
# R8), signed with the release key from ~/.gradle/gradle.properties (see
# android/app/build.gradle.kts), or the debug key without one. The APK is
# copied to dist/Rymesh-Android.apk.
android: dist
	@# libs/ is build output (ignored), so a fresh clone does not have it.
	mkdir -p android/app/libs
	@# gomobile records the source directory (a module replace) in the
	@# library's build info, which -trimpath keeps; bind through a neutral
	@# path so the APK does not carry the builder's home directory.
	mkdir -p /tmp/rymesh-build && ln -sfn "$(CURDIR)" /tmp/rymesh-build/rymesh
	cd /tmp/rymesh-build/rymesh && gomobile bind -target=android/arm64 -androidapi 26 -javapkg dev.rymesh \
		-trimpath -ldflags "$(LDFLAGS)" -o "$(CURDIR)/android/app/libs/rymesh.aar" ./mobile
	rm -rf android/app/src/main/assets/dist
	mkdir -p android/app/src/main/assets/dist
	@# Not *.gz: the Android build would unpack those into the APK.
	for a in amd64 arm64; do cp dist/rymesh_linux_$$a.gz android/app/src/main/assets/dist/rymesh_linux_$$a.gzip; done
	cd android && ./gradlew assembleRelease
	cp android/app/build/outputs/apk/release/app-release.apk dist/Rymesh-Android.apk

# macOS app: Swift shell + universal Go console, ad-hoc signed.
macos: dist
	VERSION=$(VERSION) ./macos/build-app.sh

# What a GitHub release offers for download, in dist/release/ with checksums.
release: macos android
	rm -rf dist/release && mkdir -p dist/release
	@# No extended attributes: unzip would turn them into ._ files that break the signature.
	cd macos/dist.noindex && ditto -c -k --norsrc --noextattr --noqtn --noacl --keepParent Rymesh.app ../../dist/release/Rymesh-macOS.zip
	cp dist/Rymesh-Android.apk dist/rymesh_linux_amd64 dist/rymesh_linux_arm64 dist/release/
	cd dist/release && shasum -a 256 * > SHA256SUMS
	@echo "release $(VERSION): dist/release/"; ls -l dist/release

clean:
	rm -rf dist rymesh macos/dist macos/dist.noindex macos/.build android/app/build android/app/libs android/app/src/main/assets/dist
