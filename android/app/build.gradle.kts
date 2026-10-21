plugins {
    id("com.android.application")
}

android {
    namespace = "dev.agentconsole.app"
    compileSdk = 36
    defaultConfig {
        applicationId = "dev.agentconsole.app"
        minSdk = 26
        targetSdk = 36
        versionCode = 1
        versionName = "0.1.0"
    }
    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }
}

dependencies {
    // Built by `make android`: gomobile bind of ./mobile (the Go console).
    implementation(files("libs/agentconsole.aar"))
}
