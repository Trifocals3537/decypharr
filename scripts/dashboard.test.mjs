import assert from 'node:assert/strict';
import {readFile} from 'node:fs/promises';
import test from 'node:test';
import vm from 'node:vm';

const source = await readFile(
    new URL('../pkg/server/assets/js/dashboard.js', import.meta.url),
    'utf8',
);

function dashboardPrototype() {
    const context = vm.createContext({
        URLSearchParams,
        clearTimeout,
        console,
        setInterval() {},
        setTimeout,
    });
    vm.runInContext(
        `${source}\nglobalThis.TestTorrentDashboard = TorrentDashboard;`,
        context,
        {filename: 'dashboard.js'},
    );
    return context.TestTorrentDashboard.prototype;
}

function renderProviderCell(torrent) {
    const dashboard = Object.create(dashboardPrototype());
    dashboard.escapeHtml = value => String(value);
    dashboard.state = {
        selectedEntries: new Set(),
        torrents: [{
            category: '',
            info_hash: '0123456789abcdef',
            name: 'Release',
            progress: 1,
            protocol: 'torrent',
            size: 100,
            speed: 0,
            state: 'pausedUP',
            ...torrent,
        }],
    };
    dashboard.refs = {torrentsList: {innerHTML: ''}};
    dashboard.renderTorrents();
    return dashboard.refs.torrentsList.innerHTML;
}

test('dashboard displays the active failover provider', () => {
    const html = renderProviderCell({
        active_provider: 'torbox-secondary',
        debrid: 'torbox-primary',
    });
    assert.match(html, />torbox-secondary<\/span>/);
    assert.doesNotMatch(html, />torbox-primary<\/span>/);
});

test('dashboard retains the legacy provider fallback', () => {
    const html = renderProviderCell({debrid: 'realdebrid'});
    assert.match(html, />realdebrid<\/span>/);
});
