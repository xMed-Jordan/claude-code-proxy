// icons.jsx — minimal stroked icon set
const Icon = ({ name, size = 14, ...props }) => {
  const s = size;
  const common = {
    width: s, height: s, viewBox: "0 0 16 16", fill: "none",
    stroke: "currentColor", strokeWidth: 1.4, strokeLinecap: "round", strokeLinejoin: "round",
    ...props,
  };
  const paths = {
    dashboard: <><rect x="2" y="2" width="5.5" height="6" rx="1"/><rect x="8.5" y="2" width="5.5" height="3.5" rx="1"/><rect x="2" y="9" width="5.5" height="5" rx="1"/><rect x="8.5" y="6.5" width="5.5" height="7.5" rx="1"/></>,
    config: <><path d="M2 4h6M10 4h4M2 12h2M6 12h8"/><circle cx="9" cy="4" r="1.4" fill="currentColor"/><circle cx="5" cy="12" r="1.4" fill="currentColor"/><path d="M2 8h12"/></>,
    models: <><path d="M2 4l6-2.4L14 4l-6 2.4L2 4z"/><path d="M2 8l6 2.4L14 8"/><path d="M2 12l6 2.4L14 12"/></>,
    test: <><path d="M3 2v3.5L6.5 9 3 12.5V14h10v-1.5L9.5 9 13 5.5V2H3z"/><path d="M5 4h6"/></>,
    logs: <><path d="M2.5 2.5h11v11h-11z"/><path d="M5 5l2 2-2 2M9 9h3"/></>,
    setup: <><path d="M8 1.5l5.5 3v6L8 13.5 2.5 10.5v-6L8 1.5z"/><circle cx="8" cy="7.5" r="2"/></>,
    play: <><path d="M5 3.5v9l8-4.5-8-4.5z" fill="currentColor"/></>,
    stop: <><rect x="4" y="4" width="8" height="8" rx="1" fill="currentColor"/></>,
    restart: <><path d="M3 8a5 5 0 1 1 1.5 3.5"/><path d="M3 5v3h3"/></>,
    check: <><path d="M3 8.5l3 3 7-7"/></>,
    x: <><path d="M3.5 3.5l9 9M12.5 3.5l-9 9"/></>,
    copy: <><rect x="5" y="5" width="8" height="8" rx="1.2"/><path d="M3 11V4a1 1 0 0 1 1-1h7"/></>,
    eye: <><path d="M2 8s2.5-4.5 6-4.5S14 8 14 8s-2.5 4.5-6 4.5S2 8 2 8z"/><circle cx="8" cy="8" r="1.8"/></>,
    eyeOff: <><path d="M2 8s2.5-4.5 6-4.5c1.2 0 2.3.5 3.2 1.1M14 8s-2.5 4.5-6 4.5c-1.2 0-2.3-.5-3.2-1.1"/><path d="M2 2l12 12"/></>,
    chevron: <><path d="M5 6l3 3 3-3"/></>,
    chevronR: <><path d="M6 5l3 3-3 3"/></>,
    plus: <><path d="M8 3v10M3 8h10"/></>,
    trash: <><path d="M3 4.5h10M6 4.5V3a1 1 0 0 1 1-1h2a1 1 0 0 1 1 1v1.5M5 4.5l.5 8a1 1 0 0 0 1 1h3a1 1 0 0 0 1-1l.5-8"/></>,
    edit: <><path d="M3 11.5V13h1.5L12 5.5 10.5 4 3 11.5z"/><path d="M9.5 3.5l1-1a1 1 0 0 1 1.4 0l1.6 1.6a1 1 0 0 1 0 1.4l-1 1"/></>,
    download: <><path d="M8 2v8M5 7l3 3 3-3M3 13h10"/></>,
    refresh: <><path d="M13 8a5 5 0 1 1-1.5-3.5M13 3v3h-3"/></>,
    search: <><circle cx="7" cy="7" r="4"/><path d="M10 10l3 3"/></>,
    key: <><circle cx="5.5" cy="8" r="3"/><path d="M8.5 8H14M11 8v2M13 8v2"/></>,
    lock: <><rect x="3.5" y="7" width="9" height="6" rx="1.2"/><path d="M5.5 7V5a2.5 2.5 0 0 1 5 0v2"/></>,
    bolt: <><path d="M9 1.5L3 9h4l-1 5.5L13 7H9l1-5.5z" fill="currentColor" stroke="none"/></>,
    info: <><circle cx="8" cy="8" r="6"/><path d="M8 7v4M8 5v.5"/></>,
    warn: <><path d="M8 2l6 11H2L8 2z"/><path d="M8 7v3M8 11.5v.5"/></>,
    err: <><circle cx="8" cy="8" r="6"/><path d="M5.5 5.5l5 5M10.5 5.5l-5 5"/></>,
    arrowR: <><path d="M3 8h10M9 4l4 4-4 4"/></>,
    filter: <><path d="M2 3h12l-4.5 6v4l-3 1V9L2 3z"/></>,
    pin: <><path d="M8 1.5v6M5 7.5h6l-1 3H6l-1-3zM8 10.5v4"/></>,
    book: <><path d="M2 3h5a2 2 0 0 1 2 2v8a2 2 0 0 0-2-2H2V3zM14 3H9a2 2 0 0 0-2 2v8a2 2 0 0 1 2-2h5V3z"/></>,
    folder: <><path d="M2 4a1 1 0 0 1 1-1h3l1.5 1.5H13a1 1 0 0 1 1 1V12a1 1 0 0 1-1 1H3a1 1 0 0 1-1-1V4z"/></>,
    terminal: <><rect x="2" y="3" width="12" height="10" rx="1"/><path d="M5 6.5l2 1.5-2 1.5M8.5 10h3"/></>,
  };
  return <svg className="ico" {...common}>{paths[name] || null}</svg>;
};

window.Icon = Icon;
