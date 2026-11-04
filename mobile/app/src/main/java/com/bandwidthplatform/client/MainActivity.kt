package com.bandwidthplatform.client

import android.Manifest
import android.content.Intent
import android.content.pm.PackageManager
import android.os.Build
import android.os.Bundle
import android.widget.Button
import android.widget.TextView
import androidx.appcompat.app.AppCompatActivity
import androidx.core.app.ActivityCompat
import androidx.core.content.ContextCompat

/**
 * Deliberately minimal — this reference implementation's point is the
 * background service and its policy enforcement, not UI polish. A real
 * build would add wallet/earnings display (mirroring the web wallet
 * dashboard) and settings for payout preferences here.
 */
class MainActivity : AppCompatActivity() {

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        setContentView(R.layout.activity_main)

        val statusText = findViewById<TextView>(R.id.statusText)
        val startButton = findViewById<Button>(R.id.startButton)
        val stopButton = findViewById<Button>(R.id.stopButton)

        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU) {
            if (ContextCompat.checkSelfPermission(this, Manifest.permission.POST_NOTIFICATIONS)
                != PackageManager.PERMISSION_GRANTED
            ) {
                ActivityCompat.requestPermissions(this, arrayOf(Manifest.permission.POST_NOTIFICATIONS), 1)
            }
        }

        startButton.setOnClickListener {
            ContextCompat.startForegroundService(this, Intent(this, RelayForegroundService::class.java))
            statusText.text = getString(R.string.status_running)
        }

        stopButton.setOnClickListener {
            val stopIntent = Intent(this, RelayForegroundService::class.java).apply {
                action = RelayForegroundService.ACTION_STOP
            }
            startService(stopIntent)
            statusText.text = getString(R.string.status_stopped)
        }
    }
}
