// Tells the server whether this is a phone layout (the same media query as the stylesheet), before the page is drawn: the list
// leaves out chats that nothing has grown from on a phone (SPEC 17), and its counts must be right at the first paint.
(function () {
  var mq = window.matchMedia("(max-width: 55.99rem)");
  function mark() {
    var m = document.cookie.match(/(?:^|; )one-phone=(\d)/);
    var had = m ? m[1] === "1" : false, want = mq.matches;
    document.cookie = "one-phone=" + (want ? "1" : "0") + "; path=/; max-age=31536000; samesite=lax";
    if (had !== want && document.readyState !== "loading") window.location.reload();
    return had !== want;
  }
  if (mark()) window.location.reload();
  mq.addEventListener("change", mark);
})();
