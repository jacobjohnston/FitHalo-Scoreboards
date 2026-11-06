`Archivo.woff2` is the Archivo variable font (weights 100 to 900, widths 62 to 125), the one the FitHalo
website uses. It comes from Google Fonts' repository (`ofl/archivo/Archivo[wdth,wght].ttf` in
`google/fonts`), converted with Google's `woff2_compress`, which takes it from 659 KB to 189 KB. It is
published under the SIL Open Font License 1.1, whose text is beside it as `OFL.txt`, and its notice is in
the repository's `NOTICE`.

The board serves it itself rather than loading it from Google, as the website does, so that opening a
board never tells anyone else about the visit.
