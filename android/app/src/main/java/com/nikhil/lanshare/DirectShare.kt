package com.nikhil.lanshare

import android.content.Context
import android.content.Intent
import android.content.pm.ShortcutInfo
import android.content.pm.ShortcutManager
import android.graphics.Bitmap
import android.graphics.Canvas
import android.graphics.drawable.Icon
import android.util.Log

/**
 * Paired devices as one-tap targets in the top row of every app's Share menu
 * ("Share → Nikhil PC"), so a photo open in Google Photos goes straight there.
 */
object DirectShare {
    const val CATEGORY = "com.nikhil.lanshare.category.SEND_TO_DEVICE"
    private const val PREFIX = "dev:"

    /** The device a Direct Share tap was for, if any. */
    fun deviceFor(ctx: Context, intent: Intent): Device? =
        intent.getStringExtra(Intent.EXTRA_SHORTCUT_ID)?.removePrefix(PREFIX)?.let { DeviceStore.get(ctx, it) }

    /** Re-publishes the targets (after pairing, forgetting or a rename). */
    fun update(ctx: Context) {
        runCatching {
            val sm = ctx.getSystemService(ShortcutManager::class.java) ?: return
            val devices = DeviceStore.all(ctx).take(maxOf(1, sm.maxShortcutCountPerActivity - 1))
            val shortcuts = devices.mapIndexed { i, d ->
                ShortcutInfo.Builder(ctx, PREFIX + d.id)
                    .setShortLabel(d.name.take(25))
                    .setLongLabel("Send to ${d.name}")
                    .setIcon(icon(ctx, if (d.isPhone) R.drawable.ic_phone else R.drawable.ic_laptop))
                    .setCategories(setOf(CATEGORY))
                    .setLongLived(true)
                    .setRank(i)
                    .setIntent(Intent(ctx, MainActivity::class.java).setAction(Intent.ACTION_MAIN))
                    .build()
            }
            sm.dynamicShortcuts = shortcuts
            Log.i("LanShare", "direct share: ${shortcuts.size} target(s)")
        }.onFailure { Log.w("LanShare", "direct share: $it") }
    }

    private fun icon(ctx: Context, res: Int): Icon {
        val d = ctx.resources.displayMetrics.density
        val size = (108 * d).toInt() // adaptive icon canvas; the middle 66dp shows
        val bmp = Bitmap.createBitmap(size, size, Bitmap.Config.ARGB_8888)
        val c = Canvas(bmp)
        c.drawColor(ctx.getColor(R.color.primary_container))
        ctx.getDrawable(res)?.mutate()?.apply {
            setTint(ctx.getColor(R.color.on_primary_container))
            val g = (30 * d).toInt()
            setBounds(size / 2 - g, size / 2 - g, size / 2 + g, size / 2 + g)
            draw(c)
        }
        return Icon.createWithAdaptiveBitmap(bmp)
    }
}
