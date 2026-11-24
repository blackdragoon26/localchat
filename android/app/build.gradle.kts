plugins {
    id("com.android.application")
    id("org.jetbrains.kotlin.android")
}

android {
    namespace = "local.chat.bridge"
    compileSdk = 34

    defaultConfig {
        applicationId = "local.chat.bridge"
        minSdk = 26
        targetSdk = 34
        versionCode = 1
        versionName = "0.1.0"

        ndk {
            abiFilters += listOf("arm64-v8a")
        }
    }

    buildTypes {
        release {
            isMinifyEnabled = false
        }
    }

    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }

    kotlinOptions {
        jvmTarget = "17"
    }

    packaging {
        jniLibs {
            // Extract liblocalchat.so to nativeLibraryDir so Android allows executing it.
            useLegacyPackaging = true
            keepDebugSymbols += "**/liblocalchat.so"
        }
    }
}
