plugins {
    alias(libs.plugins.android.application)
    alias(libs.plugins.kotlin.compose)
}

// A value from -P, ~/.gradle/gradle.properties or the environment, "" if none.
fun setting(prop: String, env: String): String =
    (findProperty(prop) as String?) ?: System.getenv(env) ?: ""

android {
    namespace = "chat.gliph.mobile"
    compileSdk = 37

    defaultConfig {
        applicationId = "chat.gliph.mobile"
        minSdk = 29
        targetSdk = 36
        versionCode = 1
        versionName = "0.1"
        ndk { abiFilters += listOf("arm64-v8a", "x86_64") }

        // FCM's client config. There is no google-services.json in the repo:
        // these four values are all it carries, and without them the app runs
        // with push off rather than failing to build. See android/README.md.
        buildConfigField("String", "FCM_APP_ID", "\"${setting("gliph.fcm.appId", "GLIPH_FCM_APP_ID")}\"")
        buildConfigField("String", "FCM_API_KEY", "\"${setting("gliph.fcm.apiKey", "GLIPH_FCM_API_KEY")}\"")
        buildConfigField("String", "FCM_PROJECT_ID", "\"${setting("gliph.fcm.projectId", "GLIPH_FCM_PROJECT_ID")}\"")
        buildConfigField("String", "FCM_SENDER_ID", "\"${setting("gliph.fcm.senderId", "GLIPH_FCM_SENDER_ID")}\"")
    }

    buildTypes {
        // A dev build asks for a server; a release build knows where home is.
        // Either way a server the user typed wins. Triple-tap the logo to
        // reveal the field in a pinned build.
        debug {
            buildConfigField("String", "GLIPH_SERVER", "\"${setting("gliph.server", "GLIPH_SERVER")}\"")
        }
        release {
            buildConfigField("String", "GLIPH_SERVER",
                "\"${setting("gliph.server", "GLIPH_SERVER").ifEmpty { "https://gliph.sh" }}\"")
            isMinifyEnabled = false
            signingConfig = signingConfigs.getByName("debug") // ponytail: until there is a Play upload key
        }
    }
    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }
    buildFeatures {
        compose = true
        buildConfig = true
    }
}

// The core is built by android/tools/build-core.sh into android/core and used
// in place: the .so files as jniLibs, the UniFFI Kotlin as a source dir. cargo
// is incremental, so this costs a second when core/ has not changed.
// -Pgliph.skipCore reuses what is already in android/core, for when core/ is
// mid-edit by someone else and the last good build is what you want.
val buildCore by tasks.registering(Exec::class) {
    onlyIf { findProperty("gliph.skipCore") == null }
    commandLine(rootProject.file("tools/build-core.sh").path)
}
tasks.named("preBuild") { dependsOn(buildCore) }

androidComponents {
    onVariants { variant ->
        variant.sources.jniLibs?.addStaticSourceDirectory(rootProject.file("core/jniLibs").path)
        variant.sources.kotlin?.addStaticSourceDirectory(rootProject.file("core/kotlin").path)
    }
}

dependencies {
    implementation(libs.core.ktx)
    implementation(libs.activity.compose)
    implementation(platform(libs.compose.bom))
    implementation(libs.compose.ui)
    implementation(libs.compose.foundation)
    implementation(libs.compose.material3)
    // UniFFI's generated Kotlin calls the native library through JNA.
    implementation(libs.jna) { artifact { type = "aar" } }
    implementation(libs.okhttp)
    // The device key's store: the Android counterpart of the iOS Keychain.
    implementation(libs.security.crypto)
    implementation(libs.firebase.messaging)
    // DM transfers (P2P.kt): Google's libwebrtc, so a phone and a browser speak
    // the same data channel.
    implementation(libs.webrtc)
}
