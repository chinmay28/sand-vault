package com.sandvault.tv

import android.content.Context

/**
 * The app's version, as the release workflow stamped it into versionName
 * ("v2026.11.227"), or "dev" for a local build. Read from the package rather
 * than BuildConfig, which this build does not generate.
 */
fun appVersionName(context: Context): String = try {
    @Suppress("DEPRECATION")
    context.packageManager.getPackageInfo(context.packageName, 0).versionName ?: "dev"
} catch (_: Exception) {
    "dev"
}
