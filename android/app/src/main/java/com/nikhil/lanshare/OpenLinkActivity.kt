package com.nikhil.lanshare

import android.app.Activity
import android.app.NotificationManager
import android.content.ActivityNotFoundException
import android.content.Intent
import android.net.Uri
import android.os.Bundle
import android.util.Log
import android.widget.Toast

/**
 * The clipboard notification's Open button (when what came over is a web link): opens it
 * in the browser (or the app that handles that link, like YouTube) and clears the
 * notification. No screen of its own.
 */
class OpenLinkActivity : Activity() {
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        val link = intent.getStringExtra("link").orEmpty()
        getSystemService(NotificationManager::class.java).cancel(intent.getIntExtra("note", Clip.NOTE_COPIED))
        if (!Clip.safeLink(link)) {
            Log.w("LanShare", "link: refused a non-web link")
        } else {
            Log.i("LanShare", "link: opening $link")
            try {
                startActivity(Intent(Intent.ACTION_VIEW, Uri.parse(link)).addCategory(Intent.CATEGORY_BROWSABLE)
                    .addFlags(Intent.FLAG_ACTIVITY_NEW_TASK))
            } catch (e: ActivityNotFoundException) {
                Log.w("LanShare", "link: no app opens it")
                Toast.makeText(this, "No browser on this phone to open it", Toast.LENGTH_SHORT).show()
            }
        }
        finish()
    }
}
