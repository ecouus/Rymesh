# The page calls these through window.RymeshNative.
-keepclassmembers class dev.rymesh.app.MainActivity$Bridge {
    @android.webkit.JavascriptInterface <methods>;
}
