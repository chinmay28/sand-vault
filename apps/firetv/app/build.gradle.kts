plugins {
    id("com.android.application")
    id("org.jetbrains.kotlin.android")
}

android {
    namespace = "com.sandvault.tv"
    compileSdk = 35

    defaultConfig {
        applicationId = "com.sandvault.tv"
        // Fire OS 5 (the first Fire TV Stick) is API 22; everything since is newer.
        minSdk = 22
        targetSdk = 34
        // The release workflow stamps the app with the release it ships in:
        // the vault's own version as the name, and its commit count — which
        // only ever grows — as the code, so each release installs over the
        // last. A local build is 1 / "dev".
        versionCode = (project.findProperty("sandVersionCode") as String?)?.toInt() ?: 1
        versionName = (project.findProperty("sandVersionName") as String?) ?: "dev"
    }

    buildTypes {
        release {
            isMinifyEnabled = false
            // Signed with the debug key so the release build sideloads as it
            // stands. Anybody publishing to a store signs it with their own.
            signingConfig = signingConfigs.getByName("debug")
        }
    }

    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }
    kotlinOptions {
        jvmTarget = "17"
    }
    lint {
        // A TV app has no touchscreen and no portrait mode; these checks are
        // about phones.
        disable += setOf("MissingLeanbackLauncher", "ImpliedTouchscreenHardware")
    }
}

dependencies {
    val media3 = "1.4.1"
    implementation("androidx.media3:media3-exoplayer:$media3")
    implementation("androidx.media3:media3-ui:$media3")
    implementation("androidx.annotation:annotation:1.8.2")

    testImplementation("junit:junit:4.13.2")
}
