package com.nikhil.lanshare

import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.util.Log

/**
 * Starts receiving again after the phone restarts or the app is updated, so laptops can
 * send without the user opening the app first. Does nothing until a laptop is paired.
 */
class BootReceiver : BroadcastReceiver() {
    override fun onReceive(ctx: Context, intent: Intent) {
        when (intent.action) {
            Intent.ACTION_BOOT_COMPLETED, Intent.ACTION_LOCKED_BOOT_COMPLETED, Intent.ACTION_MY_PACKAGE_REPLACED -> {
                Log.i("LanShare", "boot/update (${intent.action}): starting receiver")
                ReceiveService.start(ctx)
            }
        }
    }
}
