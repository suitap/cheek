const triggerJob = (jobName) => {
    fetch('/trigger/' + jobName, {
        method: 'POST',
        headers: {
            'Content-Type': 'application/x-www-form-urlencoded'
        },
        body: new URLSearchParams(new FormData(document.form)),
    }).then(response => {
        return response.json()
    }).then(_json => {
        window.location.reload();
    });
}