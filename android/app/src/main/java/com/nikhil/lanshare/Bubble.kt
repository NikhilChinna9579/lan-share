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
class Bubble(service: Context) {
    // Android 11+ wants windows over other apps added through a window context made for
    // that window type (a plain service context can lose them when the app leaves the screen).
    private val ctx: Context = if (android.os.Build.VERSION.SDK_INT >= 30) {
        val display = service.getSystemService(android.hardware.display.DisplayManager::class.java)
            .getDisplay(android.view.Display.DEFAULT_DISPLAY)
        service.createWindowContext(display, WindowManager.LayoutParams.TYPE_APPLICATION_OVERLAY, null)
    } else service
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
        y = prefs.getInt("y2", -1) // first time: right edge, a third down (set in show)
    }

    /** Shows or hides the bubble to match the setting and permission. */
    fun refresh() {
        val on = enabled(ctx); val allowed = Settings.canDrawOverlays(ctx)
        Log.d("LanShare", "bubble: refresh (on=$on allowed=$allowed assistant=$hiddenForAssistant shown=${view != null})")
        val away = prefs.getBoolean("dismissed", false)
        if (on && allowed && !hiddenForAssistant && !away) show() else hide()
    }

    private fun show() {
        if (view != null) return
        val s = safe()
        if (params.x < 0 || params.x > s[2] - size) params.x = s[2] - size - dp(8)
        // First time a third down; and a spot saved behind a system bar (it could be dragged
        // there before) comes back into view.
        params.y = if (params.y < 0) (s[0] + s[1]) / 3 else params.y.coerceIn(s[0], s[1])
        val v = ImageView(ctx).apply {
            setImageResource(R.drawable.ic_mic)
            imageTintList = android.content.res.ColorStateList.valueOf(ctx.getColor(R.color.on_primary))
            setBackgroundResource(R.drawable.bg_bubble)
            setPadding(dp(15), dp(15), dp(15), dp(15))
            elevation = dp(8).toFloat()
            contentDescription = "LAN Share assistant"
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
                        val (tx, ty) = targetXY()
                        Log.d("LanShare", "ui: bubble target at ${tx + size / 2},${ty + size / 2}")
                    }
                }, 600)
            }
            .onFailure {
                problem = BLOCKED
                Log.w("LanShare", "bubble: $it")
            }
    }

    fun hide() {
        hideTarget()
        view?.let { runCatching { wm.removeView(it) } }
        if (view != null) Log.i("LanShare", "bubble hidden")
        view = null
    }

    /** The area the bubble may sit in: clear of the status bar, notch and navigation bar. */
    private fun safe(): IntArray { // minY, maxY, width
        val w: Int; val h: Int; val top: Int; val bottom: Int
        if (android.os.Build.VERSION.SDK_INT >= 30) {
            val m = wm.currentWindowMetrics
            val ins = m.windowInsets.getInsetsIgnoringVisibility(
                android.view.WindowInsets.Type.systemBars() or android.view.WindowInsets.Type.displayCutout())
            w = m.bounds.width(); h = m.bounds.height(); top = ins.top; bottom = ins.bottom
        } else {
            w = ctx.resources.displayMetrics.widthPixels; h = ctx.resources.displayMetrics.heightPixels
            top = dp(32); bottom = dp(56)
        }
        return intArrayOf(top + dp(8), h - bottom - size - dp(8), w)
    }

    // The ✕ shown at the bottom while dragging: drop the bubble on it to put it away.
    private var target: ImageView? = null
    private var overTarget = false

    private fun targetXY(): Pair<Int, Int> {
        val s = safe()
        return (s[2] - size) / 2 to s[1] - dp(24)
    }

    private fun showTarget() {
        if (target != null) return
        val t = ImageView(ctx).apply {
            setImageResource(R.drawable.ic_close)
            imageTintList = android.content.res.ColorStateList.valueOf(0xFFFFFFFF.toInt())
            background = android.graphics.drawable.GradientDrawable().apply {
                shape = android.graphics.drawable.GradientDrawable.OVAL
                setColor(0xCC202124.toInt())
                setStroke(dp(2), 0xFFFFFFFF.toInt())
            }
            setPadding(dp(16), dp(16), dp(16), dp(16))
            contentDescription = "Drop here to hide the bubble"
        }
        val (tx, ty) = targetXY()
        val p = WindowManager.LayoutParams(size, size, WindowManager.LayoutParams.TYPE_APPLICATION_OVERLAY,
            WindowManager.LayoutParams.FLAG_NOT_FOCUSABLE or WindowManager.LayoutParams.FLAG_NOT_TOUCHABLE or
                WindowManager.LayoutParams.FLAG_LAYOUT_NO_LIMITS, PixelFormat.TRANSLUCENT).apply {
            gravity = Gravity.TOP or Gravity.START; x = tx; y = ty
        }
        runCatching { wm.addView(t, p); target = t; t.alpha = 0f; t.animate().alpha(1f).setDuration(120).start() }
    }

    private fun hideTarget() {
        target?.let { runCatching { wm.removeView(it) } }
        target = null
        overTarget = false
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
                    if (!moved && (abs(dx) > slop || abs(dy) > slop)) { moved = true; showTarget() }
                    if (moved) {
                        val s = safe()
                        val (tx, ty) = targetXY()
                        val x = (startX + dx.toInt()).coerceIn(0, s[2] - size)
                        val y = (startY + dy.toInt()).coerceIn(s[0], s[1])
                        // Close to the ✕: the bubble jumps onto it (as chat heads do).
                        val near = kotlin.math.hypot((x - tx).toDouble(), (y - ty).toDouble()) < dp(80)
                        if (near != overTarget) {
                            overTarget = near
                            target?.animate()?.scaleX(if (near) 1.3f else 1f)?.scaleY(if (near) 1.3f else 1f)?.setDuration(100)?.start()
                        }
                        params.x = if (near) tx else x
                        params.y = if (near) ty else y
                        runCatching { wm.updateViewLayout(v, params) }
                    }
                }
                MotionEvent.ACTION_UP, MotionEvent.ACTION_CANCEL -> {
                    v.animate().scaleX(1f).scaleY(1f).setDuration(80).start()
                    val drop = moved && overTarget
                    hideTarget()
                    when {
                        drop -> dismiss()
                        moved -> snap(v)
                        e.actionMasked == MotionEvent.ACTION_UP -> open()
                    }
                }
            }
            return true
        }
    }

    /** Dropped on the ✕: hidden until LAN Share is opened again (or switched back on). */
    private fun dismiss() {
        prefs.edit().putBoolean("dismissed", true).apply()
        hide()
        Log.i("LanShare", "bubble dismissed")
        runCatching {
            android.widget.Toast.makeText(ctx, "Bubble hidden. Open LAN Share to bring it back.", android.widget.Toast.LENGTH_LONG).show()
        }
    }

    /** Glides to the nearest side of the screen and remembers the spot. */
    private fun snap(v: View) {
        val s = safe()
        val target = if (params.x + size / 2 < s[2] / 2) dp(8) else s[2] - size - dp(8)
        params.y = params.y.coerceIn(s[0], s[1])
        ValueAnimator.ofInt(params.x, target).apply {
            duration = 180
            addUpdateListener {
                params.x = it.animatedValue as Int
                if (view === v) runCatching { wm.updateViewLayout(v, params) }
            }
            start()
        }
        prefs.edit().putInt("x", target).putInt("y2", params.y).apply()
        Log.i("LanShare", "bubble moved to $target,${params.y} (allowed y ${s[0]}..${s[1]})")
        Log.d("LanShare", "ui: bubble at ${target + size / 2},${params.y + size / 2}")
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
            ctx.getSharedPreferences("bubble", Context.MODE_PRIVATE).edit().putBoolean("on", on).putBoolean("dismissed", false).apply()
            update(ctx)
        }

        /** Opening LAN Share brings back a bubble that was dragged onto the ✕. */
        fun bringBack(ctx: Context) {
            val p = ctx.getSharedPreferences("bubble", Context.MODE_PRIVATE)
            if (p.getBoolean("dismissed", false)) {
                p.edit().putBoolean("dismissed", false).apply()
                Log.i("LanShare", "bubble back (LAN Share opened)")
            }
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
