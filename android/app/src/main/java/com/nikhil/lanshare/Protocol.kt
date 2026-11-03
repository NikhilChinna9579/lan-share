package com.nikhil.lanshare

/** Constants shared with the Windows app. Source of truth: protocol/PROTOCOL.md. */
object Protocol {
    const val VERSION = 1
    const val SERVICE_TYPE = "_lanshare._tcp"
    const val DEFAULT_PORT = 47821
    const val MAX_FRAME_SIZE = 1 shl 20 // 1 MiB
}
