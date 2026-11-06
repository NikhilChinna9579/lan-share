package com.nikhil.lanshare

import android.app.PendingIntent
import android.content.Intent
import android.os.Build
import android.service.quicksettings.Tile
import android.service.quicksettings.TileService
import android.util.Log

/**
 * Quick Settings tile "Send copied text": copy in any app, pull down the shade, tap.
 * Android lets only the app on screen read the clipboard, so the tap opens LAN Share's
 * small Send copied text window, which reads it and sends it where you choose.
 */
class ClipTile : TileService() {
    override fun onStartListening() {
        qsTile?.apply {
            state = Tile.STATE_ACTIVE // highlighted: a button that's always ready (inactive looked disabled)
            label = "Send copied text"
            if (Build.VERSION.SDK_INT >= 29) subtitle = "to your laptop"
            updateTile()
        }
    }

    override fun onClick() {
        Log.i("LanShare", "tile: send copied text")
        Clip.holdUntil = System.currentTimeMillis() + 3000 // sent once, from the window
        val i = Intent(this, ClipSendActivity::class.java)
            .addFlags(Intent.FLAG_ACTIVITY_NEW_TASK or Intent.FLAG_ACTIVITY_CLEAR_TASK)
        if (Build.VERSION.SDK_INT >= 34) {
            startActivityAndCollapse(PendingIntent.getActivity(this, 7, i, PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT))
        } else {
            @Suppress("DEPRECATION")
            startActivityAndCollapse(i)
        }
    }
}
