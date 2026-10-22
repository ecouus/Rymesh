# Release binaries for every platform a node can run on. The client uploads
# the matching one when it deploys a node over SSH (deploy.go looks in dist/).
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64
LDFLAGS := -s -w

.PHONY: build test dist android macos clean

build:
	go build -ldflags "$(LDFLAGS)" -o rymesh ./cmd/rymesh

test:
	go vet ./... && go test -race ./...

dist:
	@mkdir -p dist
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; \
		echo "building $$os/$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -ldflags "$(LDFLAGS)" -o dist/rymesh_$${os}_$${arch} ./cmd/rymesh || exit 1; \
	done

# Android app: the Go console as an AAR (gomobile) plus the Linux node
# programs it uploads when deploying to a server. Needs ANDROID_HOME,
# ANDROID_NDK_HOME and JAVA_HOME (Android Studio's JBR works).
android: dist
	gomobile bind -target=android/arm64,android/amd64 -androidapi 26 -javapkg dev.rymesh \
		-o android/app/libs/rymesh.aar ./mobile
	mkdir -p android/app/src/main/assets/dist
	cp dist/rymesh_linux_amd64 dist/rymesh_linux_arm64 android/app/src/main/assets/dist/
	cd android && ./gradlew assembleDebug

# macOS app: Swift shell + universal Go console, ad-hoc signed.
macos: dist
	./macos/build-app.sh

clean:
	rm -rf dist rymesh macos/dist macos/dist.noindex macos/.build android/app/build android/app/libs android/app/src/main/assets/dist
