try {
  var palette = localStorage.getItem("powerlinux.theme") || "moon";
  if (!["moon", "space", "violet", "pine", "sand"].includes(palette))
    palette = "moon";
  document.documentElement.dataset.palette = palette;
  document.documentElement.dataset.theme = ["space", "pine"].includes(palette)
    ? "dark"
    : "light";
} catch (_) {}
