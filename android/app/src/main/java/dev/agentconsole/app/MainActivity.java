package dev.agentconsole.app;

import android.app.Activity;
import android.content.Intent;
import android.content.res.AssetManager;
import android.graphics.Insets;
import android.net.Uri;
import android.os.Build;
import android.os.Bundle;
import android.view.View;
import android.view.WindowInsets;
import android.webkit.WebResourceRequest;
import android.webkit.WebSettings;
import android.webkit.WebView;
import android.webkit.WebViewClient;

import java.io.File;
import java.io.FileOutputStream;
import java.io.InputStream;
import java.io.OutputStream;

import dev.agentconsole.mobile.Mobile;

/**
 * One WebView over the embedded Go console (package mobile). The console is a
 * hub: it reaches the user's servers over SSH and runs no agents itself.
 */
public class MainActivity extends Activity {
    private WebView web;

    @Override
    protected void onCreate(Bundle state) {
        super.onCreate(state);
        if ((getApplicationInfo().flags & android.content.pm.ApplicationInfo.FLAG_DEBUGGABLE) != 0) {
            WebView.setWebContentsDebuggingEnabled(true);
        }
        web = new WebView(this);
        WebSettings s = web.getSettings();
        s.setJavaScriptEnabled(true);
        s.setDomStorageEnabled(true);
        web.setWebViewClient(new WebViewClient() {
            // Links out of the console (docs, URLs in answers) open in the browser.
            @Override
            public boolean shouldOverrideUrlLoading(WebView view, WebResourceRequest req) {
                Uri u = req.getUrl();
                if ("127.0.0.1".equals(u.getHost())) return false;
                startActivity(new Intent(Intent.ACTION_VIEW, u));
                return true;
            }
        });
        // Android 15+ draws edge to edge: keep the page clear of the status bar,
        // navigation bar and keyboard. A WebView ignores its own padding, so
        // the insets go on a container around it.
        android.widget.FrameLayout root = new android.widget.FrameLayout(this);
        root.setBackgroundColor(0xFFFFFFFF);
        root.addView(web);
        root.setOnApplyWindowInsetsListener((View v, WindowInsets in) -> {
            if (Build.VERSION.SDK_INT >= 30) {
                Insets i = in.getInsets(WindowInsets.Type.systemBars() | WindowInsets.Type.ime());
                v.setPadding(i.left, i.top, i.right, i.bottom);
            } else {
                v.setPadding(in.getSystemWindowInsetLeft(), in.getSystemWindowInsetTop(),
                        in.getSystemWindowInsetRight(), in.getSystemWindowInsetBottom());
            }
            return in;
        });
        setContentView(root);
        if (state != null) {
            web.restoreState(state);
            return;
        }
        new Thread(() -> {
            try {
                File files = getFilesDir();
                installNodeBinaries(new File(files, "data/dist"));
                String url = Mobile.start(files.getAbsolutePath());
                // The port only, never the ticket: logcat is readable over adb.
                android.util.Log.i("AgentConsole", "console on " + Uri.parse(url).getPort());
                runOnUiThread(() -> web.loadUrl(url));
            } catch (Exception e) {
                String html = "<p style='font:16px sans-serif;padding:24px'>启动失败：" + e.getMessage() + "</p>";
                runOnUiThread(() -> web.loadData(html, "text/html; charset=utf-8", "utf-8"));
            }
        }).start();
    }

    /**
     * Linux node programs, uploaded when the user deploys to a server. They
     * are copied out of the APK once per installed version.
     */
    private void installNodeBinaries(File dir) throws Exception {
        String stamp = String.valueOf(getPackageManager().getPackageInfo(getPackageName(), 0).lastUpdateTime);
        File marker = new File(dir, ".installed");
        if (marker.exists() && stamp.equals(new String(java.nio.file.Files.readAllBytes(marker.toPath())))) return;
        AssetManager am = getAssets();
        String[] names = am.list("dist");
        if (names == null) return;
        dir.mkdirs();
        for (String name : names) {
            try (InputStream in = am.open("dist/" + name); OutputStream os = new FileOutputStream(new File(dir, name))) {
                byte[] buf = new byte[1 << 16];
                for (int n; (n = in.read(buf)) > 0; ) os.write(buf, 0, n);
            }
        }
        java.nio.file.Files.write(marker.toPath(), stamp.getBytes());
    }

    @Override
    protected void onSaveInstanceState(Bundle out) {
        super.onSaveInstanceState(out);
        web.saveState(out);
    }

    @Override
    public void onBackPressed() {
        if (web.canGoBack()) web.goBack();
        else super.onBackPressed();
    }

    @Override
    protected void onDestroy() {
        if (isFinishing()) Mobile.stop();
        super.onDestroy();
    }
}
