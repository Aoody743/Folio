try {
  var palette = localStorage.getItem("powerlinux.theme") || "moon";
  if (!["moon", "space", "violet", "pine", "sand"].includes(palette))
    palette = "moon";
  var font = localStorage.getItem("folio.font") || "modern";
  document.documentElement.dataset.font = ["modern", "native", "reading"].includes(font) ? font : "modern";
  document.documentElement.dataset.palette = palette;
  document.documentElement.dataset.theme = ["space", "pine"].includes(palette)
    ? "dark"
    : "light";
} catch (_) {}
