package com.nikhil.lanshare

import android.content.Context
import org.json.JSONArray
import org.json.JSONObject

/**
 * A paired laptop (or phone). [id] (its key fingerprint) never changes; [host] and [port]
 * are just where it was last found, and get updated whenever discovery finds it somewhere new.
 */
data class Device(val id: String, val name: String, val host: String, val port: Int, val pairedAt: Long,
                  val platform: String = "windows") {
    val isPhone get() = platform == "android"
}

/** The list of paired laptops, saved in the app's private preferences. */
object DeviceStore {
    private const val PREFS = "devices"
    private const val KEY = "list"

    @Synchronized
    fun all(ctx: Context): List<Device> {
        migrate(ctx)
        val raw = prefs(ctx).getString(KEY, "[]")!!
        val arr = JSONArray(raw)
        return (0 until arr.length()).map {
            val o = arr.getJSONObject(it)
            Device(o.getString("id"), o.optString("name", "Laptop"), o.optString("host"),
                o.optInt("port", Protocol.DEFAULT_PORT), o.optLong("pairedAt"), o.optString("platform", "windows"))
        }.sortedBy { it.name.lowercase() }
    }

    fun get(ctx: Context, id: String): Device? = all(ctx).firstOrNull { it.id == id }

    @Synchronized
    fun put(ctx: Context, d: Device) = save(ctx, all(ctx).filter { it.id != d.id } + d)

    @Synchronized
    fun remove(ctx: Context, id: String) = save(ctx, all(ctx).filter { it.id != id })

    /** Remembers a new address for a device. No-op if unknown or unchanged. */
    @Synchronized
    fun updateAddress(ctx: Context, id: String, host: String, port: Int) {
        val d = get(ctx, id) ?: return
        if (d.host != host || d.port != port) put(ctx, d.copy(host = host, port = port))
    }

    private fun save(ctx: Context, list: List<Device>) {
        val changed = all(ctx).map { it.id to it.name } != list.map { it.id to it.name }
        val arr = JSONArray()
        list.forEach {
            arr.put(JSONObject().put("id", it.id).put("name", it.name).put("host", it.host)
                .put("port", it.port).put("pairedAt", it.pairedAt).put("platform", it.platform))
        }
        prefs(ctx).edit().putString(KEY, arr.toString()).commit()
        if (changed) DirectShare.update(ctx) // Share-menu targets follow pairing, forgetting, renames
    }

    private fun prefs(ctx: Context) = ctx.getSharedPreferences(PREFS, Context.MODE_PRIVATE)

    /** Version 0.2 saved a single laptop under "pairing"; move it into the list once. */
    private fun migrate(ctx: Context) {
        val old = ctx.getSharedPreferences("pairing", Context.MODE_PRIVATE)
        val id = old.getString("id", null) ?: return
        val d = Device(id, old.getString("name", "Laptop")!!, old.getString("host", "")!!,
            old.getInt("port", Protocol.DEFAULT_PORT), System.currentTimeMillis())
        val raw = prefs(ctx).getString(KEY, "[]")!!
        val arr = JSONArray(raw)
        if ((0 until arr.length()).none { arr.getJSONObject(it).optString("id") == id }) {
            arr.put(JSONObject().put("id", d.id).put("name", d.name).put("host", d.host)
                .put("port", d.port).put("pairedAt", d.pairedAt))
            prefs(ctx).edit().putString(KEY, arr.toString()).commit()
        }
        old.edit().clear().commit()
    }
}
