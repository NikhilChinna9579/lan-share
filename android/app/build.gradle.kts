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
        versionCode = 3
        versionName = "0.3.0"
    }

    signingConfigs {
        // A fixed debug key (committed on purpose; it protects nothing) so each new CI
        // build installs over the previous one instead of needing an uninstall.
        getByName("debug") {
            storeFile = rootProject.file("debug.keystore")
            storePassword = "android"
            keyAlias = "androiddebugkey"
            keyPassword = "android"
        }
    }

    buildTypes {
        release {
            // Shrinks and strips unused code: keeps the APK small (non-functional requirement).
            isMinifyEnabled = true
            isShrinkResources = true
            proguardFiles(getDefaultProguardFile("proguard-android-optimize.txt"))
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
}
