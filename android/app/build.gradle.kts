plugins {
    id("com.android.application")
}

android {
    namespace = "dev.rymesh.app"
    compileSdk = 36
    defaultConfig {
        applicationId = "dev.rymesh.app"
        minSdk = 26
        targetSdk = 36
        versionCode = 1
        versionName = "0.1.0"
        // The Go library is built for phones only (see the Makefile).
        ndk { abiFilters += "arm64-v8a" }
    }
    buildTypes {
        release {
            // Not debuggable: `adb shell run-as` cannot read the app's data
            // (the device list and the sealed SSH keys) and WebView debugging
            // stays off. Shrinking drops the unused library code.
            isMinifyEnabled = true
            isShrinkResources = true
            proguardFiles(getDefaultProguardFile("proguard-android-optimize.txt"), "proguard-rules.pro")
            // Signed with the debug key, like every earlier build, so it
            // installs over them and keeps the app's data.
            signingConfig = signingConfigs.getByName("debug")
        }
    }
    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }
}

dependencies {
    // Built by `make android`: gomobile bind of ./mobile (the Go console).
    implementation(files("libs/rymesh.aar"))
}
