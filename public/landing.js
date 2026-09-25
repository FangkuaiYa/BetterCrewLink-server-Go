(function() {
  var connectionsEl = document.getElementById('connections');
  var lobbiesEl = document.getElementById('lobbies');

  function updateStats() {
    fetch('/health')
      .then(function(r) { return r.json(); })
      .then(function(data) {
        if (data.connectionCount !== undefined) {
          connectionsEl.textContent = data.connectionCount;
        }
        if (data.lobbiesCount !== undefined) {
          lobbiesEl.textContent = data.lobbiesCount;
        }
      })
      .catch(function() {});
  }

  updateStats();
  setInterval(updateStats, 5000);
})();
