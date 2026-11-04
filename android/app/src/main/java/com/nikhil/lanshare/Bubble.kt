package com.nikhil.lanshare

import android.animation.ValueAnimator
import android.content.Context
import android.content.Intent
import android.graphics.PixelFormat
import android.provider.Settings
import android.util.Log
import android.view.Gravity
import android.view.MotionEvent
import android.view.View
import android.view.ViewConfiguration
import android.view.WindowManager
import android.widget.ImageView
import kotlin.math.abs

/**
 * The floating bubble: a small round button drawn over other apps. Drag it anywhere;
 * it snaps to the nearest side. Tap it to open the assistant. Lives inside the
 * receiving service, so it stays as long as LAN Share is ready to receive.
 */
class Bubble(private val ctx: Context) {
    private val wm = ctx.getSystemService(WindowManager::class.java)
    private val prefs = ctx.getSharedPreferences("bubble", Context.MODE_PRIVATE)
    private var view: ImageView? = null
    private val size = dp(56)

    private val params = WindowManager.LayoutParams(
        size, size,
        WindowManager.LayoutParams.TYPE_APPLICATION_OVERLAY,
        WindowManager.LayoutParams.FLAG_NOT_FOCUSABLE or WindowManager.LayoutParams.FLAG_LAYOUT_NO_LIMITS,
        PixelFormat.TRANSLUCENT,
    ).apply {
        gravity = Gravity.TOP or Gravity.START
        x = prefs.getInt("x", -1)
        y = prefs.getInt("y", -1) // first time: bottom right, out of the way (set in show)
    }

    /** Shows or hides the bubble to match the setting and permission. */
    fun refresh() {
        if (enabled(ctx) && Settings.canDrawOverlays(ctx) && !hiddenForAssistant) show() else hide()
    }

    private fun show() {
        if (view != null) return
        val screenW = ctx.resources.displayMetrics.widthPixels
        val screenH = ctx.resources.displayMetrics.heightPixels
        if (params.x < 0) params.x = screenW - size - dp(8)
        if (params.y < 0) params.y = screenH - size - dp(150)
        val v = ImageView(ctx).apply {
            setImageResource(R.drawable.ic_mic)
            imageTintList = android.content.res.ColorStateList.valueOf(ctx.getColor(R.color.on_primary))
            setBackgroundResource(R.drawable.bg_bubble)
            setPadding(dp(15), dp(15), dp(15), dp(15))
            elevation = dp(6).toFloat()
            contentDescription = "LAN Share assistant"
            alpha = 0.92f
        }
        v.setOnTouchListener(Drag())
        runCatching { wm.addView(v, params) }
            .onSuccess {
                view = v
                problem = null
                Log.i("LanShare", "bubble shown")
                // Some phones (Xiaomi, Oppo, Vivo…) accept the window but never draw it
                // unless a second, phone-specific permission is on: check it really appeared.
                v.postDelayed({
                    if (view !== v) return@postDelayed
                    if (!v.isAttachedToWindow || !v.isShown || v.windowVisibility != View.VISIBLE) {
                        problem = BLOCKED
                        Log.w("LanShare", "bubble: added but not visible")
                    } else {
                        val xy = IntArray(2).also { v.getLocationOnScreen(it) }
                        Log.d("LanShare", "ui: bubble at ${xy[0] + v.width / 2},${xy[1] + v.height / 2}")
                    }
                }, 600)
            }
            .onFailure {
                problem = BLOCKED
                Log.w("LanShare", "bubble: $it")
            }
    }

    fun hide() {
        view?.let { runCatching { wm.removeView(it) } }
        if (view != null) Log.i("LanShare", "bubble hidden")
        view = null
    }

    private inner class Drag : View.OnTouchListener {
        private var downX = 0f
        private var downY = 0f
        private var startX = 0
        private var startY = 0
        private var moved = false
        private val slop = ViewConfiguration.get(ctx).scaledTouchSlop

        override fun onTouch(v: View, e: MotionEvent): Boolean {
            when (e.actionMasked) {
                MotionEvent.ACTION_DOWN -> {
                    downX = e.rawX; downY = e.rawY; startX = params.x; startY = params.y; moved = false
                    v.animate().scaleX(0.9f).scaleY(0.9f).setDuration(80).start()
                }
                MotionEvent.ACTION_MOVE -> {
                    val dx = e.rawX - downX
                    val dy = e.rawY - downY
                    if (!moved && (abs(dx) > slop || abs(dy) > slop)) moved = true
                    if (moved) {
                        params.x = startX + dx.toInt()
                        params.y = (startY + dy.toInt()).coerceIn(0, ctx.resources.displayMetrics.heightPixels - size)
                        runCatching { wm.updateViewLayout(v, params) }
                    }
                }
                MotionEvent.ACTION_UP, MotionEvent.ACTION_CANCEL -> {
                    v.animate().scaleX(1f).scaleY(1f).setDuration(80).start()
                    if (moved) snap(v) else if (e.actionMasked == MotionEvent.ACTION_UP) open()
                }
            }
            return true
        }
    }

    /** Glides to the nearest side of the screen and remembers the spot. */
    private fun snap(v: View) {
        val w = ctx.resources.displayMetrics.widthPixels
        val target = if (params.x + size / 2 < w / 2) dp(8) else w - size - dp(8)
        ValueAnimator.ofInt(params.x, target).apply {
            duration = 180
            addUpdateListener {
                params.x = it.animatedValue as Int
                if (view === v) runCatching { wm.updateViewLayout(v, params) }
            }
            start()
        }
        prefs.edit().putInt("x", target).putInt("y", params.y).apply()
    }

    private fun open() {
        Log.i("LanShare", "bubble tapped")
        ctx.startActivity(Intent(ctx, AssistantActivity::class.java)
            .addFlags(Intent.FLAG_ACTIVITY_NEW_TASK or Intent.FLAG_ACTIVITY_CLEAR_TOP))
    }

    private fun dp(v: Int) = (v * ctx.resources.displayMetrics.density).toInt()

    companion object {
        /** The running service's bubble, if any. */
        @Volatile var current: Bubble? = null

        /** True while the assistant is open: the bubble steps aside. */
        @Volatile var hiddenForAssistant = false


        /**
         * On if switched on in LAN Share, or, until the person chooses either way there,
         * if they allowed "Display over other apps" (that alone says they want it).
         */
        fun enabled(ctx: Context): Boolean {
            val p = ctx.getSharedPreferences("bubble", Context.MODE_PRIVATE)
            return if (p.contains("on")) p.getBoolean("on", false) else Settings.canDrawOverlays(ctx)
        }

        fun setEnabled(ctx: Context, on: Boolean) {
            ctx.getSharedPreferences("bubble", Context.MODE_PRIVATE).edit().putBoolean("on", on).apply()
            update(ctx)
        }

        /** Why the bubble isn't showing although it's on, if we know. */
        @Volatile var problem: String? = null
        const val BLOCKED = "blocked"

        /**
         * Re-checks the bubble from any thread. The bubble lives in the receiving service:
         * if Android stopped it (or it never started), [ctx] lets us start it again.
         */
        fun update(ctx: Context? = null) {
            val b = current
            if (b == null) {
                if (ctx != null && enabled(ctx)) ReceiveService.start(ctx)
                return
            }
            android.os.Handler(android.os.Looper.getMainLooper()).post { b.refresh() }
        }

        /** One line for Settings: what the bubble is doing, or what's stopping it. */
        fun status(ctx: Context): String = when {
            !enabled(ctx) -> "A small button over other apps, for the voice assistant"
            !Settings.canDrawOverlays(ctx) -> "Needs “Display over other apps”: tap to allow"
            problem == BLOCKED -> "Your phone is blocking it: tap, then allow “Display pop-up windows while running in background” (or “Floating windows”)"
            current?.view != null -> "On: drag it anywhere; tap it in any app"
            else -> "Starting…"
        }
    }
}
