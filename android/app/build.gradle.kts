plugins {
    id("com.android.application")
    id("org.jetbrains.kotlin.android")
}

android {
    namespace = "com.nikhil.lanshare"
    compileSdk = 35

    defaultConfig {
        applicationId = "com.nikhil.lanshare"
        minSdk = 26 // Android 8.0: needed for the overlay bubble and foreground services we use
        targetSdk = 35
        versionCode = 1
        versionName = "0.1.0"
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

// No AndroidX or UI libraries yet: the plain Android framework keeps the app tiny.
dependencies {}
