plugins {
    id("com.android.application")
    id("org.jetbrains.kotlin.android")
}

android {
    namespace = "com.nikhil.lanshare"
    compileSdk = 35

    defaultConfig {
        applicationId = "com.nikhil.lanshare"
        minSdk = 29 // Android 10: first version with TLS 1.3, which the protocol requires
        targetSdk = 35
        // Every build is newer than the last (minutes since 1970) and says when it was built,
        // shown at the bottom of the home screen: easy to tell which APK is installed.
        val built = java.time.ZonedDateTime.now(java.time.ZoneId.of("Asia/Kolkata"))
        versionCode = (built.toEpochSecond() / 60).toInt()
        versionName = "0.6 · built " + built.format(java.time.format.DateTimeFormatter.ofPattern("d MMM yyyy, h:mm a", java.util.Locale.ENGLISH)) + " IST"
        // Phones (64- and 32-bit ARM) and the x86_64 emulator: keeps the wake word engine small.
        ndk { abiFilters += listOf("arm64-v8a", "armeabi-v7a", "x86_64") }
    }

    // The wake word models are memory-mapped straight from the APK.
    androidResources { noCompress += "tflite" }

    signingConfigs {
        // A fixed debug key (committed on purpose; it protects nothing) so each new CI
        // build installs over the previous one instead of needing an uninstall.
        getByName("debug") {
            storeFile = rootProject.file("debug.keystore")
            storePassword = "android"
            keyAlias = "androiddebugkey"
            keyPassword = "android"
        }
        // The real release key comes from CI secrets (never from the repo). Without them
        // a release build is simply left unsigned.
        System.getenv("RELEASE_KEYSTORE")?.takeIf { file(it).exists() }?.let { path ->
            create("release") {
                storeFile = file(path)
                storePassword = System.getenv("RELEASE_KEYSTORE_PASSWORD")
                keyAlias = System.getenv("RELEASE_KEY_ALIAS") ?: "lanshare"
                keyPassword = System.getenv("RELEASE_KEYSTORE_PASSWORD")
                storeType = "pkcs12"
            }
        }
    }

    buildTypes {
        release {
            // Shrinks and strips unused code: keeps the APK small (non-functional requirement).
            isMinifyEnabled = true
            isShrinkResources = true
            proguardFiles(getDefaultProguardFile("proguard-android-optimize.txt"))
            signingConfigs.findByName("release")?.let { signingConfig = it }
        }
    }

    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }
    kotlinOptions {
        jvmTarget = "17"
    }
}

dependencies {
    // Google's QR scanner UI, provided by Play services: no camera permission needed
    // and almost nothing added to our APK.
    implementation("com.google.android.gms:play-services-code-scanner:16.1.0")
    // "Hey Jarvis": openWakeWord models run with TensorFlow Lite (free, open source, offline).
    implementation("org.tensorflow:tensorflow-lite:2.16.1")
    // Unit tests of the command parser, run on the build machine (no emulator needed).
    testImplementation("junit:junit:4.13.2")
}
