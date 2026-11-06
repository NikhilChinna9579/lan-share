package com.nikhil.lanshare

import android.app.Activity
import android.os.Bundle
import android.os.Handler
import android.os.Looper
import android.util.Log
import android.view.Gravity
import android.view.View
import android.widget.Button
import android.widget.ImageView
import android.widget.LinearLayout
import android.widget.TextView
import android.widget.Toast

/**
 * "Send copied text" (Quick Settings tile, or the button on LAN Share's notification): a
 * small window in the middle of the screen with what's copied and each paired device
 * with a Send button. Send → it goes there (the laptop shows its Copy box) and the
 * window closes. Android lets an app read the clipboard only while it's on screen,
 * which this window is.
 */
class ClipSendActivity : Activity() {
    private val main = Handler(Looper.getMainLooper())
    private lateinit var presence: Presence
    private lateinit var list: LinearLayout
    private var text: String? = null
    private var read = false
    private var sending = false

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        setContentView(R.layout.activity_clip_send)
        setFinishOnTouchOutside(true)
        list = findViewById(R.id.clipDevices)
        findViewById<View>(R.id.clipCancel).setOnClickListener { finish() }
        presence = Presence(this, deviceName(this)) { main.post { if (!isFinishing) render() } }
    }

    override fun onStart() { super.onStart(); presence.start(); Clip.screenShown() }
    override fun onStop() { Clip.screenHidden(); presence.stop(); super.onStop() }

    override fun onWindowFocusChanged(hasFocus: Boolean) {
        super.onWindowFocusChanged(hasFocus)
        if (!hasFocus || read) return
        read = true // (only now may we read the clipboard)
        val (t, why) = Clip.read(this)
        text = t
        findViewById<TextView>(R.id.clipPreview).text = t?.let { "“" + Clip.oneLine(it) + "”" } ?: (why ?: "Nothing is copied.")
        render()
        Log.i("LanShare", "clip send: shown, ${DeviceStore.all(this).size} device(s), ${t?.length ?: 0} chars")
    }

    private fun render() {
        list.removeAllViews()
        val devices = DeviceStore.all(this)
        if (devices.isEmpty()) {
            list.addView(TextView(this).apply { text = "Pair a laptop first (open LAN Share)."; setTextColor(getColor(R.color.ink_muted)) })
            return
        }
        val d16 = (16 * resources.displayMetrics.density).toInt()
        for (d in devices) {
            val st = presence.status(d.id).state
            val row = LinearLayout(this).apply {
                orientation = LinearLayout.HORIZONTAL
                gravity = Gravity.CENTER_VERTICAL
                setPadding(0, d16 / 2, 0, d16 / 2)
            }
            row.addView(ImageView(this).apply {
                setImageResource(if (d.isPhone) R.drawable.ic_phone else R.drawable.ic_laptop)
                imageTintList = android.content.res.ColorStateList.valueOf(getColor(R.color.primary))
                layoutParams = LinearLayout.LayoutParams(d16 + d16 / 2, d16 + d16 / 2).apply { marginEnd = d16 * 3 / 4 }
            })
            row.addView(LinearLayout(this).apply {
                orientation = LinearLayout.VERTICAL
                layoutParams = LinearLayout.LayoutParams(0, LinearLayout.LayoutParams.WRAP_CONTENT, 1f)
                addView(TextView(context).apply { text = d.name; textSize = 16f; setTextColor(getColor(R.color.ink)); maxLines = 1 })
                addView(TextView(context).apply {
                    text = when (st) { Presence.State.ONLINE -> "Online"; Presence.State.CHECKING -> "Looking for it…"; else -> "Offline" }
                    textSize = 13f; setTextColor(getColor(if (st == Presence.State.ONLINE) R.color.online else R.color.ink_muted))
                })
            })
            val send = Button(this, null, 0, R.style.Button_Tonal).apply {
                text = "Send"
                isEnabled = this@ClipSendActivity.text != null && !sending
                alpha = if (isEnabled) 1f else 0.5f
                setOnClickListener { send(d) }
            }
            row.addView(send, LinearLayout.LayoutParams(LinearLayout.LayoutParams.WRAP_CONTENT, LinearLayout.LayoutParams.WRAP_CONTENT))
            if (applicationInfo.flags and android.content.pm.ApplicationInfo.FLAG_DEBUGGABLE != 0) send.post {
                val xy = IntArray(2).also { send.getLocationOnScreen(it) } // tests tap it here
                Log.d("LanShare", "ui: Send ${d.name} at ${xy[0] + send.width / 2},${xy[1] + send.height / 2}")
            }
            list.addView(row)
        }
    }

    private fun send(d: Device) {
        val t = text ?: return
        sending = true
        render()
        Log.i("LanShare", "clip send: to ${d.name}")
        Actions.sendClip(this, d, t, { e -> e.message ?: e.toString() }, auto = true) { err ->
            main.post {
                if (err == null) {
                    Toast.makeText(this, "Sent to ${d.name}", Toast.LENGTH_SHORT).show()
                    finish()
                } else {
                    sending = false
                    findViewById<TextView>(R.id.clipPreview).text = "Couldn't send to ${d.name}: $err"
                    render()
                }
            }
        }
    }
}
