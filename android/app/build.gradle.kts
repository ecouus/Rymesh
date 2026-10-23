plugins {
    id("com.android.application")
}

// One version for every program: the Go console's (internal/util/util.go).
val versionParts: List<Int> = Regex("""const Version = "(\d+)\.(\d+)\.(\d+)"""")
    .find(rootProject.file("../internal/util/util.go").readText())
    ?.groupValues?.drop(1)?.map(String::toInt)
    ?: error("no Version in internal/util/util.go")
val rymeshVersion = versionParts.joinToString(".")
val rymeshVersionCode = versionParts[0] * 10000 + versionParts[1] * 100 + versionParts[2]

// Release signing key, kept outside the repository: rymesh.keystore,
// rymesh.keyAlias, rymesh.storePassword and rymesh.keyPassword in
// ~/.gradle/gradle.properties (or -P on the command line). Without them the
// build is signed with the local debug key, so a fresh clone still builds.
fun prop(name: String): String? = providers.gradleProperty(name).orNull
val releaseKeystore = prop("rymesh.keystore")

android {
    namespace = "dev.rymesh.app"
    compileSdk = 36
    defaultConfig {
        applicationId = "dev.rymesh.app"
        minSdk = 26
        targetSdk = 36
        versionCode = rymeshVersionCode
        versionName = rymeshVersion
        // The Go library is built for phones only (see the Makefile).
        ndk { abiFilters += "arm64-v8a" }
    }
    signingConfigs {
        if (releaseKeystore != null) {
            create("release") {
                storeFile = file(releaseKeystore)
                keyAlias = prop("rymesh.keyAlias")
                storePassword = prop("rymesh.storePassword")
                keyPassword = prop("rymesh.keyPassword")
            }
        }
    }
    buildTypes {
        release {
            // Not debuggable: `adb shell run-as` cannot read the app's data
            // (the device list and the sealed SSH keys) and WebView debugging
            // stays off. Shrinking drops the unused library code.
            isMinifyEnabled = true
            isShrinkResources = true
            proguardFiles(getDefaultProguardFile("proguard-android-optimize.txt"), "proguard-rules.pro")
            signingConfig = signingConfigs.findByName("release") ?: signingConfigs.getByName("debug").also {
                logger.warn("rymesh: no release key configured, signing with the debug key")
            }
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
    // QR scanning for importing devices (ZXing; no Google Play services,
    // which many phones do not have).
    implementation("com.journeyapps:zxing-android-embedded:4.3.0")
    // The scanner's activity calls androidx.core (ContextCompat) but does not
    // declare it; without it the scanner crashes as it opens.
    implementation("androidx.core:core:1.13.1")
}
