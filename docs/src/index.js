(function () {
  document.addEventListener('DOMContentLoaded', function () {
    // Wrap HTML table into div element.
    document.querySelectorAll('.td-content table').forEach((table) => {
      // skip if already wrapped
      if (table.parentElement.classList.contains('table-wrapper')) {
        return;
      }

      const wrapper = document.createElement('div');
      wrapper.className = 'table-wrapper';

      table.parentNode.insertBefore(wrapper, table);
      wrapper.appendChild(table);
    });
  });

  /**
   * Add Image Popup.
   */
  (() => {
    const popup = document.createElement('div');
    popup.className = 'image-zoom';
    popup.innerHTML =
      '<i class="bg-drop"></i><div class="img-preview"><i class="close-btn"></i></div>';
    document.body.appendChild(popup);
    const preview = popup.querySelector('.img-preview');

    function closePopup() {
      document.body.classList.remove('image-zoom');
      popup.classList.remove('open');
      const existingImg = preview.querySelector('img');
      if (existingImg) {
        existingImg.remove();
      }
    }

    document.addEventListener('click', function (e) {
      const target = e.target.closest(
        '.td-content > img:not(.icon), .td-content p > img, .td-content table img',
      );
      if (target) {
        const imgSrc = target.getAttribute('src');
        const imgWidth = target.getAttribute('width');
        const imgHeight = target.getAttribute('height');
        const newImg = document.createElement('img');
        newImg.src = imgSrc;

        document.body.classList.add('image-zoom');
        popup.classList.add('open');
        if (imgWidth) {
          newImg.setAttribute('width', imgWidth);
        }
        if (imgHeight) {
          newImg.setAttribute('height', imgHeight);
        }
        preview.appendChild(newImg);
      }
    });

    /**
     * Close popup on clicking cross.
     */
    document.addEventListener('click', function (e) {
      if (e.target.closest('.bg-drop') || e.target.closest('.close-btn')) {
        closePopup();
      }
    });

    /**
     * Close popup on escape key.
     */
    document.onkeydown = function (event) {
      const keycode = event.keyCode ? event.keyCode : event.which;
      if (keycode === 27) {
        closePopup();
      }
    };
  })();

  /**
   * Add markdown copy functionality.
   */
  (() => {
    document
      .querySelector('.markdown-actions button.copy-link')
      .addEventListener('click', function () {
        const button = this;
        const originalHtml = button.innerHTML;
        const url = button.getAttribute('data-copy-file');

        button.innerHTML = '<i class="fa fa-spinner fa-spin me-2"></i><span>Copying...</span>';

        // Fetch the file content.
        fetch(url)
          .then((response) => {
            if (!response.ok) {
              throw new Error('File not found');
            }
            return response.text();
          })
          .then((text) => {
            // Copy content to clipboard.
            navigator.clipboard
              .writeText(text)
              .then(() => {
                button.innerHTML = '<i class="fa fa-copy me-2"></i><span>Copied</span>';
              })
              .catch(() => {
                button.innerHTML = '<i class="fa fa-copy me-2"></i><span>Failed to copy</span>';
              })
              .finally(() => {
                setTimeout(() => {
                  button.innerHTML = originalHtml;
                }, 2000);
              });
          })
          .catch((err) => {
            button.innerHTML = originalHtml;
          });
      });
  })();
})();
