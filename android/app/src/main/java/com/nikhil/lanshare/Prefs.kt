package com.nikhil.lanshare

import android.content.Context

/** This phone's own choices (Settings sheet). */
object Prefs {
    private fun p(ctx: Context) = ctx.getSharedPreferences("settings", Context.MODE_PRIVATE)

    /** May paired laptops list and get files from the shared folders? On unless turned off. */
    /** The first-start "Set up LAN Share" screen has been shown (once). */
    fun setupShown(ctx: Context) = p(ctx).getBoolean("setup_shown", false)
    fun setSetupShown(ctx: Context) = p(ctx).edit().putBoolean("setup_shown", true).apply()

    fun allowBrowse(ctx: Context) = p(ctx).getBoolean("browse", true)
    fun setAllowBrowse(ctx: Context, on: Boolean) = p(ctx).edit().putBoolean("browse", on).apply()

    /** The name laptops see; null = the phone's own name. */
    fun name(ctx: Context): String? = p(ctx).getString("name", null)?.takeIf { it.isNotBlank() }
    fun setName(ctx: Context, name: String?) = p(ctx).edit().putString("name", name?.trim()?.take(60)).apply()
}
