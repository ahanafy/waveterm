import * as mobx from "mobx";
import * as React from "react";
import {createRoot} from 'react-dom/client';
import {sprintf} from "sprintf-js";
import {Terminal} from 'xterm';
import {Main} from "./main";
import {GlobalModel} from "./model";
import {v4 as uuidv4} from "uuid";

// @ts-ignore
let VERSION = __PROMPT_VERSION__;
// @ts-ignore
let BUILD = __PROMPT_BUILD__;

let jbmFontNormal = new FontFace("JetBrains Mono", "url('static/fonts/jetbrains-mono-v13-latin-regular.woff2')", {style: "normal", weight: "400"});
let jbmFont200 = new FontFace("JetBrains Mono", "url('static/fonts/jetbrains-mono-v13-latin-200.woff2')", {style: "normal", weight: "200"});
let jbmFont700 = new FontFace("JetBrains Mono", "url('static/fonts/jetbrains-mono-v13-latin-700.woff2')", {style: "normal", weight: "700"});
let faFont = new FontFace("FontAwesome", "url(static/fonts/fontawesome-webfont-4.7.woff2)", {style: "normal", weight: "normal"});
let docFonts : any = document.fonts; // work around ts typing issue
docFonts.add(jbmFontNormal);
docFonts.add(jbmFont200);
docFonts.add(jbmFont700);
docFonts.add(faFont);
jbmFontNormal.load();
jbmFont200.load();
jbmFont700.load();
faFont.load();

document.addEventListener("DOMContentLoaded", () => {
    let reactElem = React.createElement(Main, null, null);
    let elem = document.getElementById("app");
    let root = createRoot(elem);
    let isFontLoaded = document.fonts.check("12px 'JetBrains Mono'");
    if (isFontLoaded) {
        root.render(reactElem);
    }
    else {
        document.fonts.ready.then(() => {
            root.render(reactElem);
        });
    }
});

(window as any).mobx = mobx;
(window as any).sprintf = sprintf;

console.log("PROMPT", VERSION, BUILD)
