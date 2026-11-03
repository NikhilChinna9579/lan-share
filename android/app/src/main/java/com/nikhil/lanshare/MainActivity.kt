package com.nikhil.lanshare

import android.app.Activity
import android.os.Bundle
import android.view.Gravity
import android.widget.TextView

/** Placeholder screen. The spike replaces this with pairing (QR scan) and a send button. */
class MainActivity : Activity() {
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        val version = packageManager.getPackageInfo(packageName, 0).versionName
        setContentView(TextView(this).apply {
            text = "LAN Share $version\nprotocol v${Protocol.VERSION}"
            textSize = 20f
            gravity = Gravity.CENTER
        })
    }
}
